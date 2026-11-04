package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	snapclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/prioe/pbs-volume-backup/internal/backup"
)

var errFailed = errors.New("some volumes failed")

// backupFlags parses the backup flags. Each flag defaults to the value of
// its environment variable.
func backupFlags(args []string, stderr io.Writer) (backup.Config, bool, error) {
	cfg := backup.Config{SnapshotClasses: map[string]string{}}
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Namespace, "namespace", os.Getenv("POD_NAMESPACE"), "own namespace for clones and backup pods (env POD_NAMESPACE; default: from kubeconfig or service account)")
	fs.StringVar(&cfg.Image, "image", os.Getenv("PVB_IMAGE"), "image of the backup pod (env PVB_IMAGE)")
	fs.StringVar(&cfg.Repository, "repository", os.Getenv("PBS_REPOSITORY"), "PBS repository, user@realm!token@host:datastore (env PBS_REPOSITORY)")
	fs.StringVar(&cfg.Fingerprint, "fingerprint", os.Getenv("PBS_FINGERPRINT"), "SHA-256 fingerprint of the PBS certificate (env PBS_FINGERPRINT)")
	fs.StringVar(&cfg.PBSNamespace, "pbs-namespace", os.Getenv("PBS_NAMESPACE"), "PBS namespace for all backup groups (env PBS_NAMESPACE)")
	fs.StringVar(&cfg.TokenSecret, "token-secret", os.Getenv("PVB_TOKEN_SECRET"), "Secret in the own namespace with the PBS token (env PVB_TOKEN_SECRET)")
	fs.StringVar(&cfg.TokenSecretKey, "token-secret-key", envOr("PVB_TOKEN_SECRET_KEY", "password"), "key of the token in the Secret (env PVB_TOKEN_SECRET_KEY)")
	skip := fs.String("skip-label", envOr("PVB_SKIP_LABEL", "pbs-volume-backup/skip=true"), "PVCs with this label are skipped (env PVB_SKIP_LABEL)")
	fs.Func("snapshot-class", "storage class to VolumeSnapshotClass mapping, storageclass=snapshotclass; repeatable, or comma-separated in env PVB_SNAPSHOT_CLASSES", func(s string) error {
		return addMappings(cfg.SnapshotClasses, s)
	})
	var envErrs []error
	fs.DurationVar(&cfg.SnapshotTimeout, "snapshot-timeout", envDuration("PVB_SNAPSHOT_TIMEOUT", 10*time.Minute, &envErrs), "wait at most this long for a snapshot (env PVB_SNAPSHOT_TIMEOUT)")
	fs.DurationVar(&cfg.StartTimeout, "start-timeout", envDuration("PVB_START_TIMEOUT", 15*time.Minute, &envErrs), "wait at most this long for the backup pod to start (env PVB_START_TIMEOUT)")
	fs.DurationVar(&cfg.BackupTimeout, "backup-timeout", envDuration("PVB_BACKUP_TIMEOUT", 6*time.Hour, &envErrs), "maximum run time of one backup pod (env PVB_BACKUP_TIMEOUT)")
	fs.DurationVar(&cfg.CleanupTimeout, "cleanup-timeout", envDuration("PVB_CLEANUP_TIMEOUT", 2*time.Minute, &envErrs), "maximum time for the cleanup of one volume (env PVB_CLEANUP_TIMEOUT)")
	verbose := fs.Bool("v", false, "log the client output of each backup")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "only list what would be backed up; change nothing")
	if err := fs.Parse(args); err != nil {
		return cfg, false, errUsage
	}
	if err := errors.Join(envErrs...); err != nil {
		return cfg, false, err
	}
	if fs.NArg() > 0 {
		return cfg, false, fmt.Errorf("unexpected arguments %v: %w", fs.Args(), errUsage)
	}
	if env := os.Getenv("PVB_SNAPSHOT_CLASSES"); env != "" {
		if err := addMappings(cfg.SnapshotClasses, env); err != nil {
			return cfg, false, err
		}
	}
	key, value, ok := strings.Cut(*skip, "=")
	if !ok || key == "" {
		return cfg, false, fmt.Errorf("skip label %q is not key=value", *skip)
	}
	cfg.SkipLabel, cfg.SkipValue = key, value
	return cfg, *verbose, nil
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, verbose, err := backupFlags(args, stderr)
	if err != nil {
		return err
	}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	if cfg.Namespace == "" {
		if cfg.Namespace, _, err = clientConfig.Namespace(); err != nil {
			return fmt.Errorf("own namespace: %w", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	snap, err := snapclient.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: level}))
	results, err := backup.NewDriver(kube, snap, cfg, log).Run(ctx)
	if err != nil {
		return err
	}
	counts := map[backup.Status]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	summary := []any{"ok", counts[backup.StatusOK], "failed", counts[backup.StatusFailed], "skipped", counts[backup.StatusSkipped]}
	if cfg.DryRun {
		summary = []any{"planned", counts[backup.StatusPlanned], "failed", counts[backup.StatusFailed], "skipped", counts[backup.StatusSkipped]}
	}
	log.Info("done", summary...)
	if counts[backup.StatusFailed] > 0 {
		return errFailed
	}
	return nil
}

func addMappings(m map[string]string, s string) error {
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" || v == "" {
			return fmt.Errorf("snapshot class mapping %q is not storageclass=snapshotclass", pair)
		}
		m[k] = v
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}
