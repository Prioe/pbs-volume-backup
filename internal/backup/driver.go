package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// Status is the outcome of one PVC.
type Status string

// Statuses of a PVC.
const (
	StatusOK      Status = "ok"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
	StatusPlanned Status = "planned"
)

// Result is the outcome of one PVC in a run.
type Result struct {
	Namespace string
	Name      string
	Status    Status
	Reason    string
	Err       error
	Duration  time.Duration
}

// Driver runs backups.
type Driver struct {
	kube         kubernetes.Interface
	snap         snapclient.Interface
	cfg          Config
	log          *slog.Logger
	pollInterval time.Duration
	logs         logReader
}

// NewDriver returns a driver for the given clients and config.
func NewDriver(kube kubernetes.Interface, snap snapclient.Interface, cfg Config, log *slog.Logger) *Driver {
	return &Driver{kube: kube, snap: snap, cfg: cfg, log: log, pollInterval: 2 * time.Second, logs: kubeLogs(kube)}
}

// Run removes leftovers of earlier runs, then backs up every selected PVC
// one after the other. It returns one result per PVC that is not managed by
// the tool. A dry run only reports the selection.
func (d *Driver) Run(ctx context.Context) ([]Result, error) {
	if !d.cfg.DryRun {
		if err := d.sweep(ctx); err != nil {
			return nil, fmt.Errorf("remove leftovers: %w", err)
		}
	}
	targets, results, err := d.selectTargets(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range results {
		d.report(ctx, r, nil, !d.cfg.DryRun)
	}
	if d.cfg.DryRun {
		for _, t := range targets {
			r := Result{Namespace: t.pvc.Namespace, Name: t.pvc.Name, Status: StatusPlanned, Reason: "snapshot class " + t.snapshotClass}
			d.report(ctx, r, t.pvc, false)
			results = append(results, r)
		}
		return results, nil
	}
	return append(results, d.backupAll(ctx, targets)...), nil
}

// backupAll backs up the targets with at most Concurrency at a time and
// returns their results in the order of targets.
func (d *Driver) backupAll(ctx context.Context, targets []target) []Result {
	results := make([]Result, len(targets))
	sem := make(chan struct{}, max(1, d.cfg.Concurrency))
	var wg sync.WaitGroup
	for i, t := range targets {
		if err := acquire(ctx, sem); err != nil {
			results[i] = Result{Namespace: t.pvc.Namespace, Name: t.pvc.Name, Status: StatusFailed, Err: fmt.Errorf("not attempted: %w", err)}
			d.report(ctx, results[i], t.pvc, false)
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			r := Result{Namespace: t.pvc.Namespace, Name: t.pvc.Name, Status: StatusOK}
			start := time.Now()
			r.Err = d.backupOne(ctx, t)
			r.Duration = time.Since(start)
			if r.Err != nil {
				r.Status = StatusFailed
			}
			d.report(ctx, r, t.pvc, true)
			results[i] = r
		})
	}
	wg.Wait()
	return results
}

// acquire takes a slot of sem, or fails once ctx is done.
func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sem <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-sem
			return err
		}
		return nil
	}
}

func (d *Driver) report(ctx context.Context, r Result, pvc *corev1.PersistentVolumeClaim, event bool) {
	attrs := []any{"pvc", r.Namespace + "/" + r.Name, "result", string(r.Status)}
	if r.Duration > 0 {
		attrs = append(attrs, "duration", r.Duration.Round(time.Millisecond).String())
	}
	if r.Reason != "" {
		attrs = append(attrs, "reason", r.Reason)
	}
	if r.Err != nil {
		attrs = append(attrs, "error", r.Err.Error())
		d.log.Error("volume", attrs...)
		if event {
			d.recordFailure(context.WithoutCancel(ctx), r, pvc)
		}
		return
	}
	d.log.Info("volume", attrs...)
}

func (d *Driver) recordFailure(ctx context.Context, r Result, pvc *corev1.PersistentVolumeClaim) {
	ref := corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: r.Namespace, Name: r.Name}
	if pvc != nil {
		ref.UID = pvc.UID
	}
	msg := r.Err.Error()
	if len(msg) > 1024 {
		msg = msg[:1021] + "..."
	}
	now := metav1.Now()
	prefix := r.Name
	if len(prefix) > 230 {
		prefix = prefix[:230]
	}
	ev := &corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", prefix, now.UnixNano()), Namespace: r.Namespace},
		InvolvedObject:      ref,
		Reason:              "BackupFailed",
		Message:             msg,
		Type:                corev1.EventTypeWarning,
		Source:              corev1.EventSource{Component: managedByValue},
		ReportingController: managedByValue,
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
	}
	if _, err := d.kube.CoreV1().Events(r.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		d.log.Warn("create event", "pvc", r.Namespace+"/"+r.Name, "error", err.Error())
	}
}

// backupOne runs the flow of design.md for one PVC. Every object it creates
// is deleted again before it returns, also when ctx is cancelled.
func (d *Driver) backupOne(ctx context.Context, t target) (err error) {
	var undo cleanup
	defer func() {
		cctx, cancel := d.cleanupContext(ctx)
		defer cancel()
		if cerr := undo.run(cctx); cerr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup: %w", cerr))
		}
	}()

	name := objectName(t.pvc.Namespace, t.pvc.Name)
	snaps := d.snap.SnapshotV1()

	if _, err := snaps.VolumeSnapshots(t.pvc.Namespace).Create(ctx, d.sourceSnapshot(name, t), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deleteSnapshot(ctx, t.pvc.Namespace, name) })
	source, err := d.waitSnapshot(ctx, t.pvc.Namespace, name)
	if err != nil {
		return err
	}
	content, err := snaps.VolumeSnapshotContents().Get(ctx, *source.Status.BoundVolumeSnapshotContentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get snapshot content: %w", err)
	}
	if content.Status == nil || content.Status.SnapshotHandle == nil {
		return fmt.Errorf("snapshot content %s has no snapshot handle", content.Name)
	}

	if _, err := snaps.VolumeSnapshotContents().Create(ctx, d.importedContent(name, content.Spec.Driver, *content.Status.SnapshotHandle, t), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create imported snapshot content: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deleteContent(ctx, name) })
	if _, err := snaps.VolumeSnapshots(d.cfg.Namespace).Create(ctx, d.importedSnapshot(name, t), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create imported snapshot: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deleteSnapshot(ctx, d.cfg.Namespace, name) })
	if _, err := d.waitSnapshot(ctx, d.cfg.Namespace, name); err != nil {
		return fmt.Errorf("imported %w", err)
	}

	size := t.pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if source.Status.RestoreSize != nil && !source.Status.RestoreSize.IsZero() {
		size = *source.Status.RestoreSize
	}
	if _, err := d.kube.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).Create(ctx, d.clonePVC(name, size, t), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create clone PVC: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deleteClone(ctx, name) })

	cm, err := d.metaConfigMap(name, t)
	if err != nil {
		return err
	}
	if _, err := d.kube.CoreV1().ConfigMaps(d.cfg.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create ConfigMap: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deleteConfigMap(ctx, name) })

	if _, err := d.kube.CoreV1().Pods(d.cfg.Namespace).Create(ctx, d.backupPod(name, t), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create backup pod: %w", err)
	}
	undo.push(func(ctx context.Context) error { return d.deletePod(ctx, name) })
	logs, err := waitPod(ctx, d.kube, d.logs, d.cfg.Namespace, name, "backup pod", d.cfg.StartTimeout, d.cfg.BackupTimeout, d.pollInterval)
	if err != nil {
		return err
	}
	d.log.Debug("client output", "pvc", t.pvc.Namespace+"/"+t.pvc.Name, "log", logs)
	return nil
}

// cleanupContext outlives the cancellation of ctx, so a SIGTERM during a
// backup still deletes everything.
func (d *Driver) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d.cfg.CleanupTimeout)
}

func (d *Driver) waitSnapshot(ctx context.Context, namespace, name string) (*snapv1.VolumeSnapshot, error) {
	var vs *snapv1.VolumeSnapshot
	var lastErr string
	err := wait.PollUntilContextTimeout(ctx, d.pollInterval, d.cfg.SnapshotTimeout, true, func(ctx context.Context) (bool, error) {
		var err error
		vs, err = d.snap.SnapshotV1().VolumeSnapshots(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if st := vs.Status; st != nil {
			if st.Error != nil && st.Error.Message != nil {
				lastErr = *st.Error.Message
			}
			return ptr.Deref(st.ReadyToUse, false) && st.BoundVolumeSnapshotContentName != nil, nil
		}
		return false, nil
	})
	if err != nil {
		if lastErr != "" {
			return nil, fmt.Errorf("snapshot %s/%s not ready: %w (last error: %s)", namespace, name, err, lastErr)
		}
		return nil, fmt.Errorf("snapshot %s/%s not ready: %w", namespace, name, err)
	}
	return vs, nil
}

// cleanup is a stack of undo steps, run in reverse order.
type cleanup []func(context.Context) error

func (c *cleanup) push(f func(context.Context) error) { *c = append(*c, f) }

func (c cleanup) run(ctx context.Context) error {
	var errs []error
	for i := len(c) - 1; i >= 0; i-- {
		errs = append(errs, c[i](ctx))
	}
	return errors.Join(errs...)
}

func (d *Driver) deletePod(ctx context.Context, name string) error {
	return deletePod(ctx, d.kube, d.cfg.Namespace, name)
}

func (d *Driver) deleteConfigMap(ctx context.Context, name string) error {
	return ignoreNotFound(d.kube.CoreV1().ConfigMaps(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{}))
}

// deleteClone sets the clone's PV to Delete first: with Retain, deleting the
// PV would leave the volume on the storage.
func (d *Driver) deleteClone(ctx context.Context, name string) error {
	pvcs := d.kube.CoreV1().PersistentVolumeClaims(d.cfg.Namespace)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pvc.Spec.VolumeName != "" {
		if err := d.setReclaimDelete(ctx, pvc.Spec.VolumeName); err != nil {
			return err
		}
	}
	return ignoreNotFound(pvcs.Delete(ctx, name, metav1.DeleteOptions{}))
}

func (d *Driver) setReclaimDelete(ctx context.Context, pv string) error {
	return setReclaimDelete(ctx, d.kube, pv)
}

func setReclaimDelete(ctx context.Context, kube kubernetes.Interface, pv string) error {
	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
	_, err := kube.CoreV1().PersistentVolumes().Patch(ctx, pv, types.MergePatchType, patch, metav1.PatchOptions{})
	return ignoreNotFound(err)
}

func (d *Driver) deleteSnapshot(ctx context.Context, namespace, name string) error {
	return ignoreNotFound(d.snap.SnapshotV1().VolumeSnapshots(namespace).Delete(ctx, name, metav1.DeleteOptions{}))
}

func (d *Driver) deleteContent(ctx context.Context, name string) error {
	return ignoreNotFound(d.snap.SnapshotV1().VolumeSnapshotContents().Delete(ctx, name, metav1.DeleteOptions{}))
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
