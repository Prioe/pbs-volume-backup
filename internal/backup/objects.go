package backup

import (
	"fmt"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	metaPath = "/meta"
	pvcFile  = "pvc.yaml"
)

func (d *Driver) meta(namespace, name string, t target) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      managedLabels(),
		Annotations: sourceAnnotations(t.pvc.Namespace, t.pvc.Name),
	}
}

func (d *Driver) sourceSnapshot(name string, t target) *snapv1.VolumeSnapshot {
	return &snapv1.VolumeSnapshot{
		ObjectMeta: d.meta(t.pvc.Namespace, name, t),
		Spec: snapv1.VolumeSnapshotSpec{
			Source:                  snapv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr.To(t.pvc.Name)},
			VolumeSnapshotClassName: ptr.To(t.snapshotClass),
		},
	}
}

// importedContent points at the same storage snapshot as the source
// snapshot's content. Retain keeps that snapshot when the import is deleted.
func (d *Driver) importedContent(name, driver, handle string, t target) *snapv1.VolumeSnapshotContent {
	meta := d.meta("", name, t)
	return &snapv1.VolumeSnapshotContent{
		ObjectMeta: meta,
		Spec: snapv1.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: corev1.ObjectReference{Namespace: d.cfg.Namespace, Name: name},
			DeletionPolicy:    snapv1.VolumeSnapshotContentRetain,
			Driver:            driver,
			Source:            snapv1.VolumeSnapshotContentSource{SnapshotHandle: ptr.To(handle)},
			SourceVolumeMode:  ptr.To(corev1.PersistentVolumeFilesystem),
		},
	}
}

func (d *Driver) importedSnapshot(name string, t target) *snapv1.VolumeSnapshot {
	return &snapv1.VolumeSnapshot{
		ObjectMeta: d.meta(d.cfg.Namespace, name, t),
		Spec: snapv1.VolumeSnapshotSpec{
			Source: snapv1.VolumeSnapshotSource{VolumeSnapshotContentName: ptr.To(name)},
		},
	}
}

func (d *Driver) clonePVC(name string, size resource.Quantity, t target) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: d.meta(d.cfg.Namespace, name, t),
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: t.pvc.Spec.StorageClassName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: ptr.To(snapv1.GroupName),
				Kind:     "VolumeSnapshot",
				Name:     name,
			},
		},
	}
}

// metaConfigMap holds the source PVC and PV as YAML for the pvc.conf archive.
func (d *Driver) metaConfigMap(name string, t target) (*corev1.ConfigMap, error) {
	pvc := t.pvc.DeepCopy()
	pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
	pvc.ManagedFields = nil
	pvc.ResourceVersion = ""
	pvc.Status = corev1.PersistentVolumeClaimStatus{}
	pv := t.pv.DeepCopy()
	pv.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"}
	pv.ManagedFields = nil
	pv.ResourceVersion = ""
	pv.Status = corev1.PersistentVolumeStatus{}

	pvcYAML, err := yaml.Marshal(pvc)
	if err != nil {
		return nil, fmt.Errorf("marshal PVC: %w", err)
	}
	pvYAML, err := yaml.Marshal(pv)
	if err != nil {
		return nil, fmt.Errorf("marshal PV: %w", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: d.meta(d.cfg.Namespace, name, t),
		Data:       map[string]string{pvcFile: string(pvcYAML) + "---\n" + string(pvYAML)},
	}, nil
}

func (d *Driver) backupPod(name string, t target) *corev1.Pod {
	args := []string{
		"backup",
		"volume.pxar:" + dataPath,
		"pvc.conf:" + metaPath + "/" + pvcFile,
		"--backup-type", "host",
		"--backup-id", backupID(t.pvc.Namespace, t.pvc.Name),
		"--change-detection-mode", "metadata",
	}
	return clientPod{
		meta:     d.meta(d.cfg.Namespace, name, t),
		args:     append(args, d.cfg.ExtraArgs...),
		caps:     []corev1.Capability{"DAC_OVERRIDE"},
		deadline: d.cfg.BackupTimeout,
		withKey:  true,
		volumes: []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name, ReadOnly: true},
			}},
			{Name: "meta", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}},
			}},
		},
		mounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataPath, ReadOnly: true},
			{Name: "meta", MountPath: metaPath, ReadOnly: true},
		},
	}.build(&d.cfg.Client, secrets{token: d.cfg.TokenSecret, key: d.cfg.KeySecret})
}
