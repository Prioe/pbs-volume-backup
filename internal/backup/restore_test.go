package backup

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const (
	appNS     = "app"
	targetPVC = "data"
	listJSON  = `[{"backup-type":"host","backup-id":"app.data","backup-time":1762279449},` +
		`{"backup-type":"host","backup-id":"app.data","backup-time":1762279000}]`
	latestSnapshot = "host/app.data/2025-11-04T18:04:09Z"
	pvcConf        = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  labels:
    app: demo
    app.kubernetes.io/managed-by: helm
  name: data
  namespace: app
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
  storageClassName: fast
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: pv-1
`
)

type restoreHarness struct {
	kube *kubefake.Clientset
	r    *Restorer

	mu        sync.Mutex
	failPod   string
	logsByPod map[string]string
}

func testRestoreConfig() RestoreConfig {
	return RestoreConfig{
		Client: Client{
			Namespace:      toolNS,
			Image:          "example.org/pbs-volume-backup:test",
			Repository:     "user@pbs!token@pbs.example:store",
			Fingerprint:    "aa:bb",
			PBSNamespace:   "cluster",
			TokenSecret:    "pbs-token",
			TokenSecretKey: "password",
		},
		SourceNamespace: appNS,
		SourcePVC:       targetPVC,
		TargetNamespace: appNS,
		TargetPVC:       targetPVC,
		StartTimeout:    time.Minute,
		RestoreTimeout:  time.Hour,
		CleanupTimeout:  5 * time.Second,
	}
}

func tokenSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pbs-token", Namespace: toolNS},
		Data:       map[string][]byte{"password": []byte("secret-token")},
	}
}

func newRestoreHarness(t *testing.T, cfg RestoreConfig, objs ...runtime.Object) *restoreHarness {
	t.Helper()
	h := &restoreHarness{kube: kubefake.NewClientset(objs...)}
	h.r = NewRestorer(h.kube, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	h.r.pollInterval = time.Millisecond
	h.r.logs = h.podLogs
	h.kube.PrependReactor("create", "pods", h.kubelet)
	h.kube.PrependReactor("create", "persistentvolumeclaims", h.provisioner)
	return h
}

func (h *restoreHarness) podLogs(_ context.Context, _, name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if out, ok := h.logsByPod[name]; ok {
		return out
	}
	switch {
	case strings.HasSuffix(name, "-list"):
		return "some notice\n" + listJSON + "\n"
	case strings.HasSuffix(name, "-meta"):
		return pvcConf
	default:
		return "restore done\n"
	}
}

func (h *restoreHarness) kubelet(action k8stesting.Action) (bool, runtime.Object, error) {
	pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
	h.mu.Lock()
	defer h.mu.Unlock()
	pod.Status.Phase = corev1.PodSucceeded
	if h.failPod != "" && strings.HasSuffix(pod.Name, h.failPod) {
		pod.Status.Phase = corev1.PodFailed
	}
	return false, nil, nil
}

func (h *restoreHarness) provisioner(action k8stesting.Action) (bool, runtime.Object, error) {
	pvc := action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-restored"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name},
		},
	}
	if err := h.kube.Tracker().Add(pv); err != nil {
		return true, nil, err
	}
	pvc.Spec.VolumeName = pv.Name
	return false, nil, nil
}

func (h *restoreHarness) createdPods() []*corev1.Pod {
	var pods []*corev1.Pod
	for _, a := range h.kube.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			pods = append(pods, c.GetObject().(*corev1.Pod))
		}
	}
	return pods
}

// assertNoHelpers fails if a pod or Secret of the restore is left.
func (h *restoreHarness) assertNoHelpers(t *testing.T) {
	t.Helper()
	sel := metav1.ListOptions{LabelSelector: managedSelector()}
	pods, _ := h.kube.CoreV1().Pods(metav1.NamespaceAll).List(t.Context(), sel)
	secrets, _ := h.kube.CoreV1().Secrets(metav1.NamespaceAll).List(t.Context(), sel)
	if len(pods.Items)+len(secrets.Items) > 0 {
		t.Errorf("leftovers: %d pods, %d secrets", len(pods.Items), len(secrets.Items))
	}
}

func (h *restoreHarness) target(t *testing.T) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc, err := h.kube.CoreV1().PersistentVolumeClaims(appNS).Get(t.Context(), targetPVC, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pvc
}

func TestRestoreLatestIntoNewPVC(t *testing.T) {
	h := newRestoreHarness(t, testRestoreConfig(), tokenSecret())
	snapshot, err := h.r.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != latestSnapshot {
		t.Errorf("snapshot = %s, want %s", snapshot, latestSnapshot)
	}
	h.assertNoHelpers(t)

	pvc := h.target(t)
	if pvc == nil {
		t.Fatal("target PVC not created")
	}
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if ptr.Deref(pvc.Spec.StorageClassName, "") != "fast" || size.Cmp(resource.MustParse("5Gi")) != 0 ||
		pvc.Labels["app"] != "demo" || pvc.Labels[managedByLabel] != "" ||
		pvc.Annotations[restoredFromAnnotation] != latestSnapshot {
		t.Errorf("target PVC = %+v", pvc)
	}

	pods := h.createdPods()
	if len(pods) != 3 {
		t.Fatalf("created %d pods, want list, meta and restore", len(pods))
	}
	for _, p := range pods[:2] {
		if p.Namespace != toolNS || p.Spec.Volumes[0].Secret.SecretName != "pbs-token" {
			t.Errorf("info pod %s runs in %s with secret %s", p.Name, p.Namespace, p.Spec.Volumes[0].Secret.SecretName)
		}
	}
	if args := strings.Join(pods[1].Spec.Containers[0].Args, " "); !strings.Contains(args, "restore "+latestSnapshot+" pvc.conf -") {
		t.Errorf("meta pod args = %s", args)
	}
	restore := pods[2]
	args := strings.Join(restore.Spec.Containers[0].Args, " ")
	for _, want := range []string{"restore " + latestSnapshot + " volume.pxar /data", "--allow-existing-dirs true", "--ns cluster"} {
		if !strings.Contains(args, want) {
			t.Errorf("restore args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "--overwrite") {
		t.Error("new PVC restored with --overwrite")
	}
	if restore.Namespace != appNS || restore.Spec.Containers[0].SecurityContext.Capabilities.Add[0] != "CHOWN" {
		t.Errorf("restore pod = %+v", restore.Spec)
	}
	for _, v := range restore.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "pbs-token" {
			t.Error("restore pod mounts the original Secret instead of the copy")
		}
	}
}

func TestRestoreExplicitSnapshotSkipsList(t *testing.T) {
	cfg := testRestoreConfig()
	cfg.Snapshot = "2026-11-04T19:04:09+01:00"
	h := newRestoreHarness(t, cfg, tokenSecret())
	snapshot, err := h.r.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != "host/app.data/2026-11-04T18:04:09Z" {
		t.Errorf("snapshot = %s", snapshot)
	}
	for _, p := range h.createdPods() {
		if strings.HasSuffix(p.Name, "-list") {
			t.Error("listed snapshots although one was given")
		}
	}
}

func TestRestoreOverrides(t *testing.T) {
	cfg := testRestoreConfig()
	cfg.StorageClass = "slow"
	cfg.Size = "8Gi"
	cfg.TargetPVC = "copy"
	h := newRestoreHarness(t, cfg, tokenSecret())
	if _, err := h.r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	pvc, err := h.kube.CoreV1().PersistentVolumeClaims(appNS).Get(t.Context(), "copy", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if ptr.Deref(pvc.Spec.StorageClassName, "") != "slow" || size.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("target PVC spec = %+v", pvc.Spec)
	}
}

func existingPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: targetPVC, Namespace: appNS}}
}

func TestRestoreIntoExisting(t *testing.T) {
	cfg := testRestoreConfig()
	cfg.IntoExisting = true
	h := newRestoreHarness(t, cfg, tokenSecret(), existingPVC())
	if _, err := h.r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	pods := h.createdPods()
	if args := strings.Join(pods[len(pods)-1].Spec.Containers[0].Args, " "); !strings.Contains(args, "--overwrite true") {
		t.Errorf("restore args = %s", args)
	}
	for _, a := range h.kube.Actions() {
		if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() != "get" {
			t.Errorf("existing PVC touched: %s", a.GetVerb())
		}
	}
	h.assertNoHelpers(t)
}

func TestRestoreRefuses(t *testing.T) {
	inUse := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-0", Namespace: appNS},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: targetPVC},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	tests := []struct {
		name         string
		intoExisting bool
		objs         []runtime.Object
		want         string
	}{
		{name: "target exists", objs: []runtime.Object{existingPVC()}, want: "--into-existing"},
		{name: "into missing PVC", intoExisting: true, want: "does not exist"},
		{name: "target in use", intoExisting: true, objs: []runtime.Object{existingPVC(), inUse}, want: "used by pod app-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testRestoreConfig()
			cfg.IntoExisting = tt.intoExisting
			h := newRestoreHarness(t, cfg, append(tt.objs, tokenSecret())...)
			_, err := h.r.Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if pods := h.createdPods(); len(pods) != 0 {
				t.Errorf("created %d pods", len(pods))
			}
		})
	}
}

func TestRestoreFailureRemovesNewPVC(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*restoreHarness)
		wantErr string
	}{
		{name: "restore pod fails", setup: func(h *restoreHarness) { h.failPod = dnsName(restoreNamePrefix, appNS, targetPVC, "") }, wantErr: "restore pod failed"},
		{name: "token key missing", setup: func(h *restoreHarness) { h.r.cfg.TokenSecretKey = "other" }, wantErr: `no key "other"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRestoreHarness(t, testRestoreConfig(), tokenSecret())
			tt.setup(h)
			_, err := h.r.Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if h.target(t) != nil {
				t.Error("failed restore left the new PVC")
			}
			pv, err := h.kube.CoreV1().PersistentVolumes().Get(t.Context(), "pv-restored", metav1.GetOptions{})
			if err == nil && pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				t.Error("PV of the removed PVC keeps Retain")
			}
			h.assertNoHelpers(t)
		})
	}
}

func TestRestoreFailureKeepsExistingPVC(t *testing.T) {
	cfg := testRestoreConfig()
	cfg.IntoExisting = true
	h := newRestoreHarness(t, cfg, tokenSecret(), existingPVC())
	h.failPod = dnsName(restoreNamePrefix, appNS, targetPVC, "")
	if _, err := h.r.Run(t.Context()); err == nil {
		t.Fatal("restore succeeded")
	}
	if h.target(t) == nil {
		t.Error("failed restore deleted the existing PVC")
	}
	h.assertNoHelpers(t)
}

func TestRestoreRemovesLeftovers(t *testing.T) {
	name := dnsName(restoreNamePrefix, appNS, targetPVC, "")
	h := newRestoreHarness(t, testRestoreConfig(), tokenSecret(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: managedLabels()}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: managedLabels()}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-list", Namespace: toolNS, Labels: managedLabels()}},
	)
	if _, err := h.r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.assertNoHelpers(t)
}

func TestRestoreNoSnapshots(t *testing.T) {
	h := newRestoreHarness(t, testRestoreConfig(), tokenSecret())
	h.logsByPod = map[string]string{dnsName(restoreNamePrefix, appNS, targetPVC, "-list"): "[]\n"}
	_, err := h.r.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no snapshots in group host/app.data") {
		t.Errorf("err = %v", err)
	}
	if h.target(t) != nil {
		t.Error("PVC created without a snapshot")
	}
}

func TestParsePVC(t *testing.T) {
	pvc, err := parsePVC("warning: something\n" + pvcConf)
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Name != "data" || pvc.Labels["app"] != "demo" {
		t.Errorf("pvc = %+v", pvc)
	}
	if _, err := parsePVC("connection refused\n"); err == nil {
		t.Error("parsed a PVC from an error message")
	}
	if _, err := parsePVC("apiVersion: v1\nkind: PersistentVolume\n"); err == nil {
		t.Error("accepted a PV as the PVC")
	}
}

func TestRestoreConfigValidate(t *testing.T) {
	cfg := testRestoreConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	cfg.SourcePVC = ""
	cfg.RestoreTimeout = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "source PVC") || !strings.Contains(err.Error(), "restore timeout") {
		t.Errorf("err = %v", err)
	}
}
