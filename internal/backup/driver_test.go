package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapfake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const (
	toolNS    = "pbs"
	csiDriver = "csi.example"
	storage   = "fast"
)

type harness struct {
	kube *kubefake.Clientset
	snap *snapfake.Clientset
	d    *Driver
	log  *bytes.Buffer

	mu            sync.Mutex
	podPhase      corev1.PodPhase
	snapshotReady bool
}

func testConfig() Config {
	return Config{
		Namespace:       toolNS,
		Image:           "example.org/pbs-volume-backup:test",
		Repository:      "user@pbs!token@pbs.example:store",
		Fingerprint:     "aa:bb",
		PBSNamespace:    "cluster",
		TokenSecret:     "pbs-token",
		TokenSecretKey:  "password",
		SkipLabel:       "pbs-volume-backup/skip",
		SkipValue:       "true",
		SnapshotTimeout: time.Second,
		StartTimeout:    time.Minute,
		BackupTimeout:   time.Hour,
		CleanupTimeout:  5 * time.Second,
	}
}

func newHarness(t *testing.T, cfg Config, objs ...runtime.Object) *harness {
	t.Helper()
	var kubeObjs, snapObjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *snapv1.VolumeSnapshot, *snapv1.VolumeSnapshotContent, *snapv1.VolumeSnapshotClass:
			snapObjs = append(snapObjs, o)
		default:
			kubeObjs = append(kubeObjs, o)
		}
	}
	h := &harness{
		kube: kubefake.NewClientset(kubeObjs...),
		snap: snapfake.NewSimpleClientset(snapObjs...), //nolint:staticcheck // the package has no NewClientset

		log:           &bytes.Buffer{},
		podPhase:      corev1.PodSucceeded,
		snapshotReady: true,
	}
	h.d = NewDriver(h.kube, h.snap, cfg, slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	h.d.pollInterval = time.Millisecond

	h.snap.PrependReactor("create", "volumesnapshots", h.snapshotController)
	h.kube.PrependReactor("create", "persistentvolumeclaims", h.provisioner)
	h.kube.PrependReactor("create", "pods", h.kubelet)
	return h
}

// snapshotController marks new snapshots ready. For a snapshot of a PVC it
// also creates the bound content with a handle.
func (h *harness) snapshotController(action k8stesting.Action) (bool, runtime.Object, error) {
	vs := action.(k8stesting.CreateAction).GetObject().(*snapv1.VolumeSnapshot)
	h.mu.Lock()
	ready := h.snapshotReady
	h.mu.Unlock()
	contentName := ptr.Deref(vs.Spec.Source.VolumeSnapshotContentName, "")
	if vs.Spec.Source.PersistentVolumeClaimName != nil {
		contentName = "snapcontent-" + vs.Namespace + "-" + vs.Name
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: contentName},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Driver:         csiDriver,
				DeletionPolicy: snapv1.VolumeSnapshotContentDelete,
				Source:         snapv1.VolumeSnapshotContentSource{VolumeHandle: ptr.To("vol")},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{SnapshotHandle: ptr.To("handle-" + vs.Name)},
		}
		if err := h.snap.Tracker().Add(content); err != nil {
			return true, nil, err
		}
	}
	vs.Status = &snapv1.VolumeSnapshotStatus{
		ReadyToUse:                     ptr.To(ready),
		BoundVolumeSnapshotContentName: ptr.To(contentName),
		RestoreSize:                    ptr.To(resource.MustParse("2Gi")),
	}
	return false, nil, nil
}

// provisioner binds clone PVCs to a new PV with reclaim policy Retain.
func (h *harness) provisioner(action k8stesting.Action) (bool, runtime.Object, error) {
	pvc := action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
	if pvc.Namespace != toolNS {
		return false, nil, nil
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + pvc.Name},
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

func (h *harness) kubelet(action k8stesting.Action) (bool, runtime.Object, error) {
	pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
	h.mu.Lock()
	pod.Status.Phase = h.podPhase
	h.mu.Unlock()
	if pod.Status.Phase == corev1.PodFailed {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "backup",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 255}},
		}}
	}
	return false, nil, nil
}

// failOn makes the n-th call of verb on resource fail.
func failOn(f *k8stesting.Fake, verb, res string, n int) {
	var calls int
	var mu sync.Mutex
	f.PrependReactor(verb, res, func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == n {
			return true, nil, fmt.Errorf("injected %s %s failure", verb, res)
		}
		return false, nil, nil
	})
}

func boundPVC(namespace, name string, labels map[string]string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvName := "pv-" + namespace + "-" + name
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, UID: types.UID("uid-" + name)},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(storage),
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: csiDriver, VolumeHandle: "vol-" + name},
			},
			ClaimRef: &corev1.ObjectReference{Namespace: namespace, Name: name},
		},
	}
	return pvc, pv
}

func defaultClass(name, driver string) *snapv1.VolumeSnapshotClass {
	return &snapv1.VolumeSnapshotClass{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Annotations: map[string]string{defaultSnapshotClassAnnotation: "true"}},
		Driver:         driver,
		DeletionPolicy: snapv1.VolumeSnapshotContentDelete,
	}
}

func appObjects() []runtime.Object {
	pvc, pv := boundPVC("app", "data", nil)
	return []runtime.Object{pvc, pv, defaultClass("default", csiDriver)}
}

// assertClean fails if any object with the managed-by label is left, or a
// clone PV still has reclaim policy Retain.
func (h *harness) assertClean(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	sel := metav1.ListOptions{LabelSelector: managedSelector()}
	var left []string
	pods, _ := h.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, sel)
	for _, o := range pods.Items {
		left = append(left, "pod "+o.Name)
	}
	cms, _ := h.kube.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, sel)
	for _, o := range cms.Items {
		left = append(left, "configmap "+o.Name)
	}
	pvcs, _ := h.kube.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, sel)
	for _, o := range pvcs.Items {
		left = append(left, "pvc "+o.Name)
	}
	vss, _ := h.snap.SnapshotV1().VolumeSnapshots(metav1.NamespaceAll).List(ctx, sel)
	for _, o := range vss.Items {
		left = append(left, "volumesnapshot "+o.Namespace+"/"+o.Name)
	}
	contents, _ := h.snap.SnapshotV1().VolumeSnapshotContents().List(ctx, sel)
	for _, o := range contents.Items {
		left = append(left, "volumesnapshotcontent "+o.Name)
	}
	pvs, _ := h.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	for _, pv := range pvs.Items {
		if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == toolNS &&
			pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			left = append(left, "retained pv "+pv.Name)
		}
	}
	if len(left) > 0 {
		t.Errorf("leftovers: %s", strings.Join(left, ", "))
	}
}

func (h *harness) failureEvents(t *testing.T, namespace string) []corev1.Event {
	t.Helper()
	evs, err := h.kube.CoreV1().Events(namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []corev1.Event
	for _, e := range evs.Items {
		if e.Reason == "BackupFailed" {
			out = append(out, e)
		}
	}
	return out
}

func createdPod(t *testing.T, h *harness) *corev1.Pod {
	t.Helper()
	for _, a := range h.kube.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			return c.GetObject().(*corev1.Pod)
		}
	}
	t.Fatal("no pod created")
	return nil
}

func TestRunSuccess(t *testing.T) {
	h := newHarness(t, testConfig(), appObjects()...)
	results, err := h.d.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != StatusOK {
		t.Fatalf("results = %+v", results)
	}
	h.assertClean(t)
	if evs := h.failureEvents(t, "app"); len(evs) != 0 {
		t.Errorf("unexpected events: %+v", evs)
	}
	if !strings.Contains(h.log.String(), "pvc=app/data result=ok") {
		t.Errorf("log has no result line:\n%s", h.log)
	}

	name := objectName("app", "data")
	content, err := h.snap.Tracker().Get(snapv1.SchemeGroupVersion.WithResource("volumesnapshotcontents"), "", name)
	if err == nil {
		t.Errorf("imported content still exists: %v", content)
	}
	var imported *snapv1.VolumeSnapshotContent
	for _, a := range h.snap.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "volumesnapshotcontents" {
			imported = c.GetObject().(*snapv1.VolumeSnapshotContent)
		}
	}
	if imported == nil {
		t.Fatal("no content imported")
	}
	if imported.Spec.DeletionPolicy != snapv1.VolumeSnapshotContentRetain ||
		ptr.Deref(imported.Spec.Source.SnapshotHandle, "") != "handle-"+name ||
		imported.Spec.VolumeSnapshotRef.Namespace != toolNS || imported.Spec.Driver != csiDriver {
		t.Errorf("imported content = %+v", imported.Spec)
	}
}

func TestBackupPodSpec(t *testing.T) {
	h := newHarness(t, testConfig(), appObjects()...)
	if _, err := h.d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	pod := createdPod(t, h)
	if pod.Namespace != toolNS {
		t.Errorf("pod namespace = %s", pod.Namespace)
	}
	c := pod.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"volume.pxar:/data", "pvc.conf:/meta/pvc.yaml", "--backup-type host",
		"--backup-id app.data", "--change-detection-mode metadata", "--ns cluster",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	for _, v := range c.Env {
		if v.Name == "PBS_PASSWORD" {
			t.Error("token passed in environment")
		}
	}
	sc := c.SecurityContext
	if ptr.Deref(sc.AllowPrivilegeEscalation, true) || len(sc.Capabilities.Add) != 1 ||
		sc.Capabilities.Add[0] != "DAC_OVERRIDE" || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context = %+v", sc)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "data" && !m.ReadOnly {
			t.Error("data mounted read-write")
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			t.Error("PVC volume not read-only")
		}
	}
}

func TestRunFailureCleansUp(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*harness)
		ctx     func(context.Context) (context.Context, context.CancelFunc)
		wantErr string
	}{
		{
			name:    "source snapshot create",
			setup:   func(h *harness) { failOn(&h.snap.Fake, "create", "volumesnapshots", 1) },
			wantErr: "create snapshot",
		},
		{
			name: "snapshot never ready",
			setup: func(h *harness) {
				h.snapshotReady = false
				h.d.cfg.SnapshotTimeout = 20 * time.Millisecond
			},
			wantErr: "not ready",
		},
		{
			name:    "content import",
			setup:   func(h *harness) { failOn(&h.snap.Fake, "create", "volumesnapshotcontents", 1) },
			wantErr: "create imported snapshot content",
		},
		{
			name:    "imported snapshot create",
			setup:   func(h *harness) { failOn(&h.snap.Fake, "create", "volumesnapshots", 2) },
			wantErr: "create imported snapshot",
		},
		{
			name:    "clone create",
			setup:   func(h *harness) { failOn(&h.kube.Fake, "create", "persistentvolumeclaims", 1) },
			wantErr: "create clone PVC",
		},
		{
			name:    "configmap create",
			setup:   func(h *harness) { failOn(&h.kube.Fake, "create", "configmaps", 1) },
			wantErr: "create ConfigMap",
		},
		{
			name:    "pod create",
			setup:   func(h *harness) { failOn(&h.kube.Fake, "create", "pods", 1) },
			wantErr: "create backup pod",
		},
		{
			name:    "client fails",
			setup:   func(h *harness) { h.podPhase = corev1.PodFailed },
			wantErr: "exit code 255",
		},
		{
			name: "pod never starts",
			setup: func(h *harness) {
				h.podPhase = corev1.PodPending
				h.d.cfg.StartTimeout = 20 * time.Millisecond
			},
			wantErr: "backup pod not started",
		},
		{
			name:  "cancelled while pod runs",
			setup: func(h *harness) { h.podPhase = corev1.PodPending },
			ctx: func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(ctx, 50*time.Millisecond)
			},
			wantErr: "context deadline exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testConfig(), appObjects()...)
			tt.setup(h)
			ctx := t.Context()
			if tt.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = tt.ctx(ctx)
				defer cancel()
			}
			results, err := h.d.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Status != StatusFailed {
				t.Fatalf("results = %+v", results)
			}
			if !strings.Contains(results[0].Err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want %q", results[0].Err, tt.wantErr)
			}
			h.assertClean(t)
			evs := h.failureEvents(t, "app")
			if len(evs) != 1 || evs[0].InvolvedObject.Name != "data" || evs[0].Type != corev1.EventTypeWarning {
				t.Errorf("events = %+v", evs)
			}
		})
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.DryRun = true
	nfs, nfsPV := boundPVC("app", "nfs", nil)
	nfsPV.Spec.CSI = nil
	leftover := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: objectName("app", "data"), Namespace: toolNS, Labels: managedLabels()}}
	h := newHarness(t, cfg, append(appObjects(), nfs, nfsPV, leftover)...)

	results, err := h.d.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range results {
		got[r.Name] = string(r.Status) + " " + r.Reason
	}
	if got["data"] != "planned snapshot class default" || !strings.HasPrefix(got["nfs"], "failed") || len(got) != 2 {
		t.Errorf("results = %v", got)
	}
	for _, a := range append(h.kube.Actions(), h.snap.Actions()...) {
		if v := a.GetVerb(); v != "list" && v != "get" && v != "watch" {
			t.Errorf("dry run did %s %s", v, a.GetResource().Resource)
		}
	}
}

func TestCancelSkipsRemainingWithoutEvents(t *testing.T) {
	first, firstPV := boundPVC("app", "a", nil)
	second, secondPV := boundPVC("app", "b", nil)
	h := newHarness(t, testConfig(), first, firstPV, second, secondPV, defaultClass("default", csiDriver))
	h.podPhase = corev1.PodPending
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	results, err := h.d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[1].Status != StatusFailed || !strings.Contains(results[1].Err.Error(), "not attempted") {
		t.Fatalf("results = %+v", results)
	}
	h.assertClean(t)
	evs := h.failureEvents(t, "app")
	if len(evs) != 1 || evs[0].InvolvedObject.Name != "a" {
		t.Errorf("events = %+v", evs)
	}
}

func TestSelectTargets(t *testing.T) {
	ok, okPV := boundPVC("app", "ok", nil)
	optOut, optOutPV := boundPVC("app", "opt-out", map[string]string{"pbs-volume-backup/skip": "true"})
	pending, _ := boundPVC("app", "pending", nil)
	pending.Status.Phase = corev1.ClaimPending
	own, ownPV := boundPVC("other", "clone", managedLabels())
	inTool, inToolPV := boundPVC(toolNS, "x", nil)
	block, blockPV := boundPVC("app", "block", nil)
	block.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
	nfs, nfsPV := boundPVC("app", "nfs", nil)
	nfsPV.Spec.CSI = nil
	nfsPV.Spec.NFS = &corev1.NFSVolumeSource{Server: "nfs.example", Path: "/"}
	other, otherPV := boundPVC("app", "other-driver", nil)
	otherPV.Spec.CSI.Driver = "csi.other"
	mapped, mappedPV := boundPVC("app", "mapped", nil)
	mapped.Spec.StorageClassName = ptr.To("special")

	h := newHarness(t, func() Config {
		c := testConfig()
		c.SnapshotClasses = map[string]string{"special": "special-class"}
		return c
	}(),
		ok, okPV, optOut, optOutPV, pending, own, ownPV, inTool, inToolPV, block, blockPV,
		nfs, nfsPV, other, otherPV, mapped, mappedPV,
		defaultClass("default", csiDriver), defaultClass("unrelated", "csi.unrelated"),
	)
	targets, results, err := h.d.selectTargets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tg := range targets {
		got[tg.pvc.Name] = "target " + tg.snapshotClass
	}
	for _, r := range results {
		got[r.Name] = string(r.Status)
	}
	want := map[string]string{
		"ok":           "target default",
		"mapped":       "target special-class",
		"opt-out":      "skipped",
		"pending":      "skipped",
		"block":        "failed",
		"nfs":          "failed",
		"other-driver": "failed",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	for _, r := range results {
		if r.Status == StatusFailed && !strings.Contains(r.Err.Error(), "pbs-volume-backup/skip=true") {
			t.Errorf("%s: error %q does not name the skip label", r.Name, r.Err)
		}
	}
}

func TestSeveralDefaultClasses(t *testing.T) {
	objs := append(appObjects(), defaultClass("second", csiDriver))
	h := newHarness(t, testConfig(), objs...)
	_, results, err := h.d.selectTargets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Err.Error(), "several default") {
		t.Errorf("results = %+v", results)
	}
}

func TestSweepRemovesLeftovers(t *testing.T) {
	name := objectName("app", "data")
	meta := func(ns string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: managedLabels()}
	}
	clonePV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-clone"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: toolNS, Name: name},
		},
	}
	released := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-released"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: toolNS, Name: namePrefix + "gone"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	foreign := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: "app", Name: "old"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	h := newHarness(t, testConfig(),
		&corev1.Pod{ObjectMeta: meta(toolNS)},
		&corev1.ConfigMap{ObjectMeta: meta(toolNS)},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta(toolNS), Spec: corev1.PersistentVolumeClaimSpec{VolumeName: clonePV.Name}},
		clonePV, released, foreign,
		&snapv1.VolumeSnapshot{ObjectMeta: meta(toolNS)},
		&snapv1.VolumeSnapshot{ObjectMeta: meta("app")},
		&snapv1.VolumeSnapshotContent{ObjectMeta: meta("")},
	)
	results, err := h.d.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v", results)
	}
	h.assertClean(t)
	pv, err := h.kube.CoreV1().PersistentVolumes().Get(t.Context(), foreign.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Error("sweep changed a PV outside the tool namespace")
	}
}

func TestCleanupContextOutlivesCancel(t *testing.T) {
	h := newHarness(t, testConfig())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cctx, ccancel := h.d.cleanupContext(ctx)
	defer ccancel()
	if err := cctx.Err(); err != nil {
		t.Errorf("cleanup context is done: %v", err)
	}
	if _, ok := cctx.Deadline(); !ok {
		t.Error("cleanup context has no deadline")
	}
}

func TestSweepKeepsRestorePods(t *testing.T) {
	restorePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: dnsName(restoreNamePrefix, "app", "data", "-list"), Namespace: toolNS, Labels: managedLabels(),
	}}
	h := newHarness(t, testConfig(), restorePod)
	if _, err := h.d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.kube.CoreV1().Pods(toolNS).Get(t.Context(), restorePod.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("sweep deleted a restore pod: %v", err)
	}
}

func TestSweepErrorStopsRun(t *testing.T) {
	h := newHarness(t, testConfig(), appObjects()...)
	h.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := h.d.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "remove leftovers") {
		t.Errorf("err = %v", err)
	}
}

func TestObjectName(t *testing.T) {
	long := strings.Repeat("a", 60)
	tests := []struct{ ns, pvc string }{
		{"app", "data"},
		{"app", "data.v1"},
		{long, long},
		{"a-b", "c"},
		{"a", "b-c"},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		got := objectName(tt.ns, tt.pvc)
		if len(got) > maxNameLen || strings.Contains(got, ".") || !strings.HasPrefix(got, namePrefix) {
			t.Errorf("objectName(%q, %q) = %q", tt.ns, tt.pvc, got)
		}
		if got != objectName(tt.ns, tt.pvc) {
			t.Errorf("objectName(%q, %q) is not stable", tt.ns, tt.pvc)
		}
		if seen[got] {
			t.Errorf("objectName(%q, %q) = %q collides", tt.ns, tt.pvc, got)
		}
		seen[got] = true
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := testConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	cfg.Repository = ""
	cfg.BackupTimeout = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "repository") || !strings.Contains(err.Error(), "backup timeout") {
		t.Errorf("err = %v", err)
	}

	dry := Config{Namespace: toolNS, SkipLabel: "skip", DryRun: true, SnapshotTimeout: 1, StartTimeout: 1, BackupTimeout: 1, CleanupTimeout: 1}
	if err := dry.Validate(); err != nil {
		t.Errorf("dry run without PBS settings: %v", err)
	}
}
