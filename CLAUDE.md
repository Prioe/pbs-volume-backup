# CLAUDE.md

pbs-volume-backup backs up every Kubernetes PVC to a Proxmox Backup Server
through CSI snapshots. It ships as a Go binary in a container image plus a
Helm chart.

`docs/design.md` holds the requirements and the design. Read it before
changing behavior, and update it in the same change when the design moves.

## Layout (planned)

- `cmd/pbs-volume-backup/`: main package, subcommands `backup`, `restore`.
- `internal/`: driver logic (selection, snapshot, clone, pod, cleanup).
- `charts/pbs-volume-backup/`: Helm chart.
- `Dockerfile`: image with the binary and the official
  `proxmox-backup-client-static` package (pinned version and SHA-256).
- `.goreleaser.yaml`: binary and image; the release workflow also pushes the
  chart as OCI.

## Rules

- This repo is public. Never commit tokens, fingerprints of real servers,
  hostnames or IPs of a real deployment. Examples use `pbs.example`.
- Never log the PBS token or put it into a command line; pass it as a file
  (`PBS_PASSWORD_FILE`).
- Objects the tool creates carry the label `app.kubernetes.io/managed-by:
  pbs-volume-backup`, and cleanup runs on every exit path.
- Go: client-go, no kubectl. In tests, use `t.Context()`.
- Keep the chart's values documented in `values.yaml`; the README links to
  it rather than repeating it.
