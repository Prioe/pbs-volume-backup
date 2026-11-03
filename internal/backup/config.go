// Package backup implements the driver that backs up PVCs to a Proxmox
// Backup Server through CSI snapshots.
package backup

import (
	"errors"
	"time"
)

// Config holds the settings of a backup run.
type Config struct {
	// Namespace is the tool's own namespace, where clones and backup pods run.
	Namespace string
	Image     string

	Repository   string
	Fingerprint  string
	PBSNamespace string
	// TokenSecret names the Secret in Namespace that holds the PBS token.
	TokenSecret    string
	TokenSecretKey string

	SkipLabel string
	SkipValue string
	// SnapshotClasses maps storage class names to VolumeSnapshotClass names.
	SnapshotClasses map[string]string

	SnapshotTimeout time.Duration
	// StartTimeout limits how long the backup pod may stay Pending, before
	// BackupTimeout applies.
	StartTimeout   time.Duration
	BackupTimeout  time.Duration
	CleanupTimeout time.Duration
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	for _, f := range []struct{ name, value string }{
		{"namespace", c.Namespace},
		{"image", c.Image},
		{"repository", c.Repository},
		{"fingerprint", c.Fingerprint},
		{"token secret", c.TokenSecret},
		{"token secret key", c.TokenSecretKey},
		{"skip label", c.SkipLabel},
	} {
		if f.value == "" {
			errs = append(errs, errors.New(f.name+" is required"))
		}
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"snapshot timeout", c.SnapshotTimeout},
		{"start timeout", c.StartTimeout},
		{"backup timeout", c.BackupTimeout},
		{"cleanup timeout", c.CleanupTimeout},
	} {
		if d.value <= 0 {
			errs = append(errs, errors.New(d.name+" must be positive"))
		}
	}
	return errors.Join(errs...)
}
