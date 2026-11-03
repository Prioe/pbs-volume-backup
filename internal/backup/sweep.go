package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sweep deletes the objects that a crashed earlier run left behind, in the
// same order as the per-PVC cleanup.
func (d *Driver) sweep(ctx context.Context) error {
	sel := metav1.ListOptions{LabelSelector: managedSelector()}
	core := d.kube.CoreV1()
	snaps := d.snap.SnapshotV1()
	var errs []error

	pods, err := core.Pods(d.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		errs = append(errs, d.deletePod(ctx, p.Name))
	}
	cms, err := core.ConfigMaps(d.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list ConfigMaps: %w", err)
	}
	for _, cm := range cms.Items {
		errs = append(errs, d.deleteConfigMap(ctx, cm.Name))
	}
	pvcs, err := core.PersistentVolumeClaims(d.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list PVCs: %w", err)
	}
	for _, pvc := range pvcs.Items {
		errs = append(errs, d.deleteClone(ctx, pvc.Name))
	}
	errs = append(errs, d.reclaimReleased(ctx))

	imports, err := snaps.VolumeSnapshots(d.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list imported snapshots: %w", err)
	}
	for _, vs := range imports.Items {
		errs = append(errs, d.deleteSnapshot(ctx, vs.Namespace, vs.Name))
	}
	contents, err := snaps.VolumeSnapshotContents().List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list snapshot contents: %w", err)
	}
	for _, c := range contents.Items {
		errs = append(errs, d.deleteContent(ctx, c.Name))
	}
	sources, err := snaps.VolumeSnapshots(metav1.NamespaceAll).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	for _, vs := range sources.Items {
		if vs.Namespace != d.cfg.Namespace {
			errs = append(errs, d.deleteSnapshot(ctx, vs.Namespace, vs.Name))
		}
	}
	return errors.Join(errs...)
}

// reclaimReleased sets released clone PVs to Delete. A clone PVC deleted
// while its volume was still being provisioned leaves such a PV behind.
func (d *Driver) reclaimReleased(ctx context.Context) error {
	pvs, err := d.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PVs: %w", err)
	}
	var errs []error
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if pv.Status.Phase != corev1.VolumeReleased || ref == nil || ref.Namespace != d.cfg.Namespace ||
			!strings.HasPrefix(ref.Name, namePrefix) || pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
			continue
		}
		errs = append(errs, d.setReclaimDelete(ctx, pv.Name))
	}
	return errors.Join(errs...)
}
