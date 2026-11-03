// Command pbs-volume-backup backs up Kubernetes PVCs to a Proxmox Backup
// Server through CSI snapshots.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: pbs-volume-backup <command> [flags]

Commands:
  backup      back up all selected PVCs
  restore     restore a PBS snapshot into a PVC
  run-client  run proxmox-backup-client inside a backup or restore pod
  version     print the version
`

var errUsage = errors.New("invalid usage")

func main() {
	err := run(os.Args[1:], os.Stdout)
	if errors.Is(err, errUsage) {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "backup", "restore", "run-client":
		return fmt.Errorf("%s: not implemented", args[0])
	case "version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	default:
		return fmt.Errorf("unknown command %q: %w", args[0], errUsage)
	}
}
