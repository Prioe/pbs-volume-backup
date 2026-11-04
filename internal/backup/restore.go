package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	restoredFromAnnotation = "pbs-volume-backup/restored-from"
	snapshotTimeLayout     = "2006-01-02T15:04:05Z"
)

// restoreCaps let root restore owners, modes, file capabilities and device
// nodes. All of them are allowed under Pod Security baseline.
var restoreCaps = []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "MKNOD", "SETFCAP"}

// Restorer restores a PBS snapshot of a PVC into a PVC.
type Restorer struct {
	kube         kubernetes.Interface
	cfg          RestoreConfig
	log          *slog.Logger
	pollInterval time.Duration
	logs         logReader
}

// NewRestorer returns a restorer for the given client and config.
func NewRestorer(kube kubernetes.Interface, cfg RestoreConfig, log *slog.Logger) *Restorer {
	return &Restorer{kube: kube, cfg: cfg, log: log, pollInterval: 2 * time.Second, logs: kubeLogs(kube)}
}

// Run restores the snapshot. It creates the target PVC from the archived
// pvc.conf unless IntoExisting is set, and deletes that PVC again if the
// restore fails. It returns the restored snapshot path.
func (r *Restorer) Run(ctx context.Context) (snapshot string, err error) {
	c := &r.cfg
	core := r.kube.CoreV1()
	target := c.TargetNamespace + "/" + c.TargetPVC
	name := dnsName(restoreNamePrefix, c.TargetNamespace, c.TargetPVC, "")
	listName := dnsName(restoreNamePrefix, c.TargetNamespace, c.TargetPVC, "-list")
	metaName := dnsName(restoreNamePrefix, c.TargetNamespace, c.TargetPVC, "-meta")

	_, err = core.PersistentVolumeClaims(c.TargetNamespace).Get(ctx, c.TargetPVC, metav1.GetOptions{})
	exists := err == nil
	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return "", fmt.Errorf("get PVC %s: %w", target, err)
	case exists && !c.IntoExisting:
		return "", fmt.Errorf("PVC %s exists; scale its app to zero and delete the PVC, or restore with --into-existing", target)
	case !exists && c.IntoExisting:
		return "", fmt.Errorf("PVC %s does not exist, but --into-existing needs it", target)
	case exists:
		if err := r.checkUnused(ctx); err != nil {
			return "", err
		}
	}

	var undo cleanup
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.CleanupTimeout)
		defer cancel()
		if cerr := undo.run(cctx); cerr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup: %w", cerr))
		}
	}()

	if err := r.removeLeftovers(ctx, name, listName, metaName); err != nil {
		return "", fmt.Errorf("remove leftovers of an earlier restore: %w", err)
	}
	if err := r.removeStaleTokens(ctx); err != nil {
		return "", fmt.Errorf("remove token copies of earlier restores: %w", err)
	}

	snapshot, err = r.resolveSnapshot(ctx, listName, &undo)
	if err != nil {
		return "", err
	}
	r.log.Info("restoring", "snapshot", snapshot, "pvc", target)
	logs, err := r.runInfoPod(ctx, metaName, []string{"restore", snapshot, "pvc.conf", "-"}, &undo)
	if err != nil {
		return "", fmt.Errorf("read pvc.conf: %w", err)
	}
	source, err := parsePVC(logs)
	if err != nil {
		return "", err
	}

	if !exists {
		pvc, perr := r.targetPVC(source, snapshot)
		if perr != nil {
			return "", perr
		}
		if _, err := core.PersistentVolumeClaims(c.TargetNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("create PVC %s: %w", target, err)
		}
		undo.push(func(ctx context.Context) error {
			if err == nil {
				return nil
			}
			return r.deleteTarget(ctx)
		})
	}

	if err := r.copyToken(ctx, name); err != nil {
		return "", err
	}
	undo.push(func(ctx context.Context) error {
		return ignoreNotFound(core.Secrets(c.TargetNamespace).Delete(ctx, name, metav1.DeleteOptions{}))
	})

	if _, err := core.Pods(c.TargetNamespace).Create(ctx, r.restorePod(name, snapshot), metav1.CreateOptions{}); err != nil {
		if apierrors.IsForbidden(err) {
			return "", fmt.Errorf("create restore pod (namespace %s must allow Pod Security baseline): %w", c.TargetNamespace, err)
		}
		return "", fmt.Errorf("create restore pod: %w", err)
	}
	undo.push(func(ctx context.Context) error { return deletePod(ctx, r.kube, c.TargetNamespace, name) })
	logs, err = waitPod(ctx, r.kube, r.logs, c.TargetNamespace, name, "restore pod", c.StartTimeout, c.RestoreTimeout, r.pollInterval)
	if err != nil {
		return "", err
	}
	r.log.Debug("client output", "log", logs)
	return snapshot, nil
}

// checkUnused fails if a running pod mounts the target PVC.
func (r *Restorer) checkUnused(ctx context.Context) error {
	pods, err := r.kube.CoreV1().Pods(r.cfg.TargetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == r.cfg.TargetPVC {
				return fmt.Errorf("PVC %s/%s is used by pod %s; scale its app to zero first", r.cfg.TargetNamespace, r.cfg.TargetPVC, p.Name)
			}
		}
	}
	return nil
}

// removeLeftovers deletes the pods and Secret of an interrupted restore of
// the same target and waits until they are gone, since this run reuses the
// names.
func (r *Restorer) removeLeftovers(ctx context.Context, name, listName, metaName string) error {
	c := &r.cfg
	core := r.kube.CoreV1()
	type object struct {
		namespace, name string
		secret          bool
	}
	var errs []error
	for _, o := range []object{
		{c.Namespace, listName, false},
		{c.Namespace, metaName, false},
		{c.TargetNamespace, name, false},
		{c.TargetNamespace, name, true},
	} {
		get := func(ctx context.Context) error {
			_, err := core.Pods(o.namespace).Get(ctx, o.name, metav1.GetOptions{})
			return err
		}
		del := func(ctx context.Context) error { return deletePod(ctx, r.kube, o.namespace, o.name) }
		if o.secret {
			get = func(ctx context.Context) error {
				_, err := core.Secrets(o.namespace).Get(ctx, o.name, metav1.GetOptions{})
				return err
			}
			del = func(ctx context.Context) error {
				return ignoreNotFound(core.Secrets(o.namespace).Delete(ctx, o.name, metav1.DeleteOptions{}))
			}
		}
		if err := get(ctx); apierrors.IsNotFound(err) {
			continue
		}
		if err := del(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, wait.PollUntilContextTimeout(ctx, r.pollInterval, c.CleanupTimeout, true, func(ctx context.Context) (bool, error) {
			err := get(ctx)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}))
	}
	return errors.Join(errs...)
}

// removeStaleTokens deletes token copies that killed restores left in any
// namespace. A copy whose restore pod is still pending or running belongs to
// a concurrent restore and stays.
func (r *Restorer) removeStaleTokens(ctx context.Context) error {
	core := r.kube.CoreV1()
	sel := metav1.ListOptions{LabelSelector: managedSelector()}
	secrets, err := core.Secrets(metav1.NamespaceAll).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list Secrets: %w", err)
	}
	var errs []error
	for _, s := range secrets.Items {
		if !strings.HasPrefix(s.Name, restoreNamePrefix) {
			continue
		}
		pod, err := core.Pods(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
		switch {
		case err == nil && (pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning):
			continue
		case err != nil && !apierrors.IsNotFound(err):
			errs = append(errs, err)
			continue
		}
		r.log.Info("deleting token copy of an earlier restore", "secret", s.Namespace+"/"+s.Name)
		errs = append(errs, ignoreNotFound(core.Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})))
	}
	return errors.Join(errs...)
}

// resolveSnapshot returns the snapshot path. Without a configured time, it
// pins the latest snapshot of the group, so pvc.conf and the volume come
// from the same snapshot.
func (r *Restorer) resolveSnapshot(ctx context.Context, listName string, undo *cleanup) (string, error) {
	group := "host/" + backupID(r.cfg.SourceNamespace, r.cfg.SourcePVC)
	if r.cfg.Snapshot != "" {
		t, err := time.Parse(time.RFC3339, r.cfg.Snapshot)
		if err != nil {
			return "", fmt.Errorf("snapshot %q is not a time like 2026-11-04T18:04:09Z: %w", r.cfg.Snapshot, err)
		}
		return group + "/" + t.UTC().Format(snapshotTimeLayout), nil
	}
	logs, err := r.runInfoPod(ctx, listName, []string{"snapshot", "list", group, "--output-format", "json"}, undo)
	if err != nil {
		return "", fmt.Errorf("list snapshots of %s: %w", group, err)
	}
	start, end := strings.Index(logs, "["), strings.LastIndex(logs, "]")
	if start < 0 || end < start {
		return "", fmt.Errorf("list snapshots of %s: no JSON in output: %s", group, lastLines(logs, 5))
	}
	var list []struct {
		BackupTime int64 `json:"backup-time"`
	}
	if err := json.Unmarshal([]byte(logs[start:end+1]), &list); err != nil {
		return "", fmt.Errorf("list snapshots of %s: %w", group, err)
	}
	var latest int64
	for _, s := range list {
		latest = max(latest, s.BackupTime)
	}
	if latest == 0 {
		return "", fmt.Errorf("no snapshots in group %s", group)
	}
	return group + "/" + time.Unix(latest, 0).UTC().Format(snapshotTimeLayout), nil
}

// runInfoPod runs the client in the tool's namespace, where the token
// Secret is, and returns its output.
func (r *Restorer) runInfoPod(ctx context.Context, name string, args []string, undo *cleanup) (string, error) {
	c := &r.cfg
	pod := clientPod{
		meta:     metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: managedLabels()},
		args:     args,
		deadline: c.StartTimeout,
	}.build(&c.Client, c.TokenSecret)
	if _, err := r.kube.CoreV1().Pods(c.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create pod: %w", err)
	}
	undo.push(func(ctx context.Context) error { return deletePod(ctx, r.kube, c.Namespace, name) })
	return waitPod(ctx, r.kube, r.logs, c.Namespace, name, "pod "+name, c.StartTimeout, c.StartTimeout, r.pollInterval)
}

// parsePVC reads the PVC, the first document of pvc.conf, from client output.
func parsePVC(logs string) (*corev1.PersistentVolumeClaim, error) {
	start := strings.Index(logs, "apiVersion:")
	if start < 0 {
		return nil, fmt.Errorf("pvc.conf: no PVC in output: %s", lastLines(logs, 5))
	}
	doc, _, _ := strings.Cut(logs[start:], "\n---")
	var pvc corev1.PersistentVolumeClaim
	if err := yaml.Unmarshal([]byte(doc), &pvc); err != nil {
		return nil, fmt.Errorf("pvc.conf: %w", err)
	}
	if pvc.Kind != "PersistentVolumeClaim" {
		return nil, fmt.Errorf("pvc.conf: first document is a %q, not a PersistentVolumeClaim", pvc.Kind)
	}
	return &pvc, nil
}

func (r *Restorer) targetPVC(source *corev1.PersistentVolumeClaim, snapshot string) (*corev1.PersistentVolumeClaim, error) {
	c := &r.cfg
	labels := maps.Clone(source.Labels)
	delete(labels, managedByLabel)
	storageClass := source.Spec.StorageClassName
	if c.StorageClass != "" {
		storageClass = ptr.To(c.StorageClass)
	}
	size := source.Spec.Resources.Requests[corev1.ResourceStorage]
	if c.Size != "" {
		q, err := resource.ParseQuantity(c.Size)
		if err != nil {
			return nil, fmt.Errorf("size %q: %w", c.Size, err)
		}
		size = q
	}
	if size.IsZero() {
		return nil, errors.New("pvc.conf has no storage request; pass a size")
	}
	accessModes := source.Spec.AccessModes
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        c.TargetPVC,
			Namespace:   c.TargetNamespace,
			Labels:      labels,
			Annotations: map[string]string{restoredFromAnnotation: snapshot},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: storageClass,
			AccessModes:      accessModes,
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}, nil
}

// deleteTarget removes a PVC that this restore created but could not fill.
func (r *Restorer) deleteTarget(ctx context.Context) error {
	pvcs := r.kube.CoreV1().PersistentVolumeClaims(r.cfg.TargetNamespace)
	pvc, err := pvcs.Get(ctx, r.cfg.TargetPVC, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pvc.Spec.VolumeName != "" {
		if err := setReclaimDelete(ctx, r.kube, pvc.Spec.VolumeName); err != nil {
			return err
		}
	}
	return ignoreNotFound(pvcs.Delete(ctx, r.cfg.TargetPVC, metav1.DeleteOptions{}))
}

func (r *Restorer) copyToken(ctx context.Context, name string) error {
	c := &r.cfg
	src, err := r.kube.CoreV1().Secrets(c.Namespace).Get(ctx, c.TokenSecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get token Secret: %w", err)
	}
	token, ok := src.Data[c.TokenSecretKey]
	if !ok {
		return fmt.Errorf("token secret %s/%s has no key %q", c.Namespace, c.TokenSecret, c.TokenSecretKey)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.TargetNamespace, Labels: managedLabels()},
		Data:       map[string][]byte{c.TokenSecretKey: token},
	}
	if _, err := r.kube.CoreV1().Secrets(c.TargetNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("copy token Secret: %w", err)
	}
	return nil
}

func (r *Restorer) restorePod(name, snapshot string) *corev1.Pod {
	c := &r.cfg
	args := []string{"restore", snapshot, "volume.pxar", dataPath, "--allow-existing-dirs", "true"}
	if c.IntoExisting {
		args = append(args, "--overwrite", "true")
	}
	return clientPod{
		meta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.TargetNamespace,
			Labels:      managedLabels(),
			Annotations: map[string]string{restoredFromAnnotation: snapshot},
		},
		args:     args,
		caps:     restoreCaps,
		deadline: c.RestoreTimeout,
		volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.TargetPVC},
		}}},
		mounts: []corev1.VolumeMount{{Name: "data", MountPath: dataPath}},
	}.build(&c.Client, name)
}
