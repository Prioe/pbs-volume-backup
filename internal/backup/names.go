package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "pbs-volume-backup"

	sourceNamespaceAnnotation = "pbs-volume-backup/source-namespace"
	sourcePVCAnnotation       = "pbs-volume-backup/source-pvc"

	namePrefix        = "pvb-"
	restoreNamePrefix = "pvr-"
	maxNameLen        = 63
	hashLen           = 10
)

// objectName returns the name of all objects created for one PVC. It is a
// DNS label, unique per namespace and PVC, and stable across runs.
func objectName(namespace, pvc string) string {
	return dnsName(namePrefix, namespace, pvc, "")
}

// dnsName returns prefix, a readable part and a hash of namespace and PVC,
// and suffix, shortened to a DNS label.
func dnsName(prefix, namespace, pvc, suffix string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + pvc))
	hash := hex.EncodeToString(sum[:])[:hashLen]
	base := prefix + strings.ReplaceAll(namespace+"-"+pvc, ".", "-")
	if limit := maxNameLen - 1 - hashLen - len(suffix); len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	return base + "-" + hash + suffix
}

// backupID returns the PBS backup ID of a PVC. Namespaces can't contain a
// dot, so the ID is unambiguous.
func backupID(namespace, pvc string) string {
	return namespace + "." + pvc
}

func managedLabels() map[string]string {
	return map[string]string{managedByLabel: managedByValue}
}

func sourceAnnotations(namespace, pvc string) map[string]string {
	return map[string]string{
		sourceNamespaceAnnotation: namespace,
		sourcePVCAnnotation:       pvc,
	}
}

func managedSelector() string {
	return managedByLabel + "=" + managedByValue
}
