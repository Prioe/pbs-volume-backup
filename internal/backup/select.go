package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultSnapshotClassAnnotation = "snapshot.storage.kubernetes.io/is-default-class"

type target struct {
	pvc           *corev1.PersistentVolumeClaim
	pv            *corev1.PersistentVolume
	snapshotClass string
}

// selectTargets lists all PVCs and sorts them into targets and results for
// PVCs that are skipped or can't be backed up.
func (d *Driver) selectTargets(ctx context.Context) ([]target, []Result, error) {
	pvcs, err := d.kube.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list PVCs: %w", err)
	}
	pvs, err := d.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list PVs: %w", err)
	}
	classes, err := d.snap.SnapshotV1().VolumeSnapshotClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list VolumeSnapshotClasses: %w", err)
	}
	pvByName := make(map[string]*corev1.PersistentVolume, len(pvs.Items))
	for i := range pvs.Items {
		pvByName[pvs.Items[i].Name] = &pvs.Items[i]
	}

	items := pvcs.Items
	slices.SortFunc(items, func(a, b corev1.PersistentVolumeClaim) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	var targets []target
	var results []Result
	for i := range items {
		pvc := &items[i]
		if pvc.Namespace == d.cfg.Namespace || pvc.Labels[managedByLabel] == managedByValue {
			continue
		}
		if pvc.Labels[d.cfg.SkipLabel] == d.cfg.SkipValue {
			results = append(results, skipped(pvc, "opt-out label"))
			continue
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			results = append(results, skipped(pvc, "not bound"))
			continue
		}
		pv := pvByName[pvc.Spec.VolumeName]
		t, err := d.newTarget(pvc, pv, classes.Items)
		if err != nil {
			results = append(results, Result{
				Namespace: pvc.Namespace,
				Name:      pvc.Name,
				Status:    StatusFailed,
				Err:       fmt.Errorf("%w; label the PVC %s=%s to skip it", err, d.cfg.SkipLabel, d.cfg.SkipValue),
			})
			continue
		}
		targets = append(targets, t)
	}
	return targets, results, nil
}

func (d *Driver) newTarget(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, classes []snapv1.VolumeSnapshotClass) (target, error) {
	if pv == nil {
		return target{}, fmt.Errorf("PV %q not found", pvc.Spec.VolumeName)
	}
	if pv.Spec.CSI == nil {
		return target{}, fmt.Errorf("PV %s is not a CSI volume", pv.Name)
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return target{}, fmt.Errorf("block volumes are not supported")
	}
	class, err := d.snapshotClass(pvc, pv.Spec.CSI.Driver, classes)
	if err != nil {
		return target{}, err
	}
	return target{pvc: pvc, pv: pv, snapshotClass: class}, nil
}

// snapshotClass returns the mapped class for the PVC's storage class, or else
// the default class of the PV's CSI driver.
func (d *Driver) snapshotClass(pvc *corev1.PersistentVolumeClaim, driver string, classes []snapv1.VolumeSnapshotClass) (string, error) {
	if sc := pvc.Spec.StorageClassName; sc != nil {
		if class, ok := d.cfg.SnapshotClasses[*sc]; ok {
			return class, nil
		}
	}
	var defaults []string
	for _, c := range classes {
		if c.Driver == driver && c.Annotations[defaultSnapshotClassAnnotation] == "true" {
			defaults = append(defaults, c.Name)
		}
	}
	switch len(defaults) {
	case 0:
		return "", fmt.Errorf("no default VolumeSnapshotClass for driver %s", driver)
	case 1:
		return defaults[0], nil
	default:
		return "", fmt.Errorf("several default VolumeSnapshotClasses for driver %s: %s", driver, strings.Join(defaults, ", "))
	}
}

func skipped(pvc *corev1.PersistentVolumeClaim, reason string) Result {
	return Result{Namespace: pvc.Namespace, Name: pvc.Name, Status: StatusSkipped, Reason: reason}
}
