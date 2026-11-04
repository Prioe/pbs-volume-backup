package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/prioe/pbs-volume-backup/internal/backup"
)

const restoreUsage = `Usage: pbs-volume-backup restore --from <namespace>/<pvc> [--to <namespace>/<pvc>] [flags]

Restores a backup of a PVC into a new PVC, or with --into-existing into a PVC
that already exists. The target must not be in use: scale its app to zero.
Runs with your kubeconfig.

Flags:
`

func restoreFlags(args []string, stderr io.Writer) (backup.RestoreConfig, bool, error) {
	var cfg backup.RestoreConfig
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, restoreUsage)
		fs.PrintDefaults()
	}
	clientFlags(fs, &cfg.Client, "namespace of the tool, with the token Secret")
	from := fs.String("from", "", "PVC whose backup to restore, namespace/pvc")
	to := fs.String("to", "", "PVC to restore into, namespace/pvc (default: --from)")
	fs.StringVar(&cfg.Snapshot, "snapshot", "", "backup time of the snapshot, like 2026-11-04T18:04:09Z (default: latest)")
	fs.StringVar(&cfg.StorageClass, "storage-class", "", "storage class of the new PVC (default: from the backup)")
	fs.StringVar(&cfg.Size, "size", "", "size of the new PVC (default: from the backup)")
	fs.BoolVar(&cfg.IntoExisting, "into-existing", false, "restore into an existing PVC; files from the backup overwrite existing ones")
	clientArgsFlag(fs, &cfg.ExtraArgs, "PVB_RESTORE_CLIENT_ARGS", "extra option for proxmox-backup-client restore, like --rate=50MiB")
	fs.DurationVar(&cfg.StartTimeout, "start-timeout", 15*time.Minute, "wait at most this long for each pod to start")
	fs.DurationVar(&cfg.RestoreTimeout, "restore-timeout", 6*time.Hour, "maximum run time of the restore pod")
	fs.DurationVar(&cfg.CleanupTimeout, "cleanup-timeout", 2*time.Minute, "maximum time for the cleanup")
	verbose := fs.Bool("v", false, "log the client output")
	if err := fs.Parse(args); err != nil {
		return cfg, false, errUsage
	}
	if fs.NArg() > 0 {
		return cfg, false, fmt.Errorf("unexpected arguments %v: %w", fs.Args(), errUsage)
	}
	var errs []error
	if *from == "" {
		errs = append(errs, errors.New("--from is required"))
	} else if ns, pvc, err := splitPVC(*from); err != nil {
		errs = append(errs, fmt.Errorf("--from: %w", err))
	} else {
		cfg.SourceNamespace, cfg.SourcePVC = ns, pvc
		cfg.TargetNamespace, cfg.TargetPVC = ns, pvc
	}
	if *to != "" {
		if ns, pvc, err := splitPVC(*to); err != nil {
			errs = append(errs, fmt.Errorf("--to: %w", err))
		} else {
			cfg.TargetNamespace, cfg.TargetPVC = ns, pvc
		}
	}
	return cfg, *verbose, errors.Join(errs...)
}

func splitPVC(s string) (string, string, error) {
	ns, pvc, ok := strings.Cut(s, "/")
	if !ok || ns == "" || pvc == "" || strings.Contains(pvc, "/") {
		return "", "", fmt.Errorf("%q is not namespace/pvc", s)
	}
	return ns, pvc, nil
}

func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, verbose, err := restoreFlags(args, stderr)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	restConfig, err := kubeClientConfig().ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	log := logger(stdout, verbose)
	snapshot, err := backup.NewRestorer(kube, cfg, log).Run(ctx)
	if err != nil {
		return err
	}
	log.Info("restored", "snapshot", snapshot, "pvc", cfg.TargetNamespace+"/"+cfg.TargetPVC)
	return nil
}
