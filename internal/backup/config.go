// Package backup implements the backup driver and the restore of PVCs to and
// from a Proxmox Backup Server.
package backup

import (
	"errors"
	"time"
)

// Client holds the settings for pods that run proxmox-backup-client.
type Client struct {
	// Namespace is the tool's own namespace. It holds the token Secret, and
	// clones and backup pods run there.
	Namespace string
	Image     string

	Repository   string
	Fingerprint  string
	PBSNamespace string
	// TokenSecret names the Secret in Namespace that holds the PBS token.
	TokenSecret    string
	TokenSecretKey string
}

func (c *Client) validate(needPBS bool) []error {
	required := []struct{ name, value string }{{"namespace", c.Namespace}}
	if needPBS {
		required = append(required, []struct{ name, value string }{
			{"image", c.Image},
			{"repository", c.Repository},
			{"fingerprint", c.Fingerprint},
			{"token secret", c.TokenSecret},
			{"token secret key", c.TokenSecretKey},
		}...)
	}
	var errs []error
	for _, f := range required {
		if f.value == "" {
			errs = append(errs, errors.New(f.name+" is required"))
		}
	}
	return errs
}

// Config holds the settings of a backup run.
type Config struct {
	Client

	SkipLabel string
	SkipValue string
	// SnapshotClasses maps storage class names to VolumeSnapshotClass names.
	SnapshotClasses map[string]string

	SnapshotTimeout time.Duration
	// StartTimeout limits how long a pod may stay Pending, before
	// BackupTimeout applies.
	StartTimeout   time.Duration
	BackupTimeout  time.Duration
	CleanupTimeout time.Duration

	// DryRun only reports the selection and changes nothing in the cluster.
	DryRun bool
}

// Validate reports missing or invalid settings. A dry run needs only the
// settings that the selection uses.
func (c *Config) Validate() error {
	errs := c.validate(!c.DryRun)
	if c.SkipLabel == "" {
		errs = append(errs, errors.New("skip label is required"))
	}
	errs = append(errs, positive([]duration{
		{"snapshot timeout", c.SnapshotTimeout},
		{"start timeout", c.StartTimeout},
		{"backup timeout", c.BackupTimeout},
		{"cleanup timeout", c.CleanupTimeout},
	})...)
	return errors.Join(errs...)
}

// RestoreConfig holds the settings of a restore.
type RestoreConfig struct {
	Client

	SourceNamespace string
	SourcePVC       string
	TargetNamespace string
	TargetPVC       string
	// Snapshot is a backup time like 2026-11-04T18:04:09Z, or empty for the
	// latest snapshot.
	Snapshot string
	// StorageClass and Size override the values from pvc.conf.
	StorageClass string
	Size         string
	// IntoExisting restores into a PVC that already exists instead of
	// creating it.
	IntoExisting bool

	StartTimeout   time.Duration
	RestoreTimeout time.Duration
	CleanupTimeout time.Duration
}

// Validate reports missing or invalid settings.
func (c *RestoreConfig) Validate() error {
	errs := c.validate(true)
	for _, f := range []struct{ name, value string }{
		{"source namespace", c.SourceNamespace},
		{"source PVC", c.SourcePVC},
		{"target namespace", c.TargetNamespace},
		{"target PVC", c.TargetPVC},
	} {
		if f.value == "" {
			errs = append(errs, errors.New(f.name+" is required"))
		}
	}
	errs = append(errs, positive([]duration{
		{"start timeout", c.StartTimeout},
		{"restore timeout", c.RestoreTimeout},
		{"cleanup timeout", c.CleanupTimeout},
	})...)
	return errors.Join(errs...)
}

type duration struct {
	name  string
	value time.Duration
}

func positive(durations []duration) []error {
	var errs []error
	for _, d := range durations {
		if d.value <= 0 {
			errs = append(errs, errors.New(d.name+" must be positive"))
		}
	}
	return errs
}
