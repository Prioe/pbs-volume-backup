# CLAUDE.md

pbs-volume-backup backs up every Kubernetes PVC to a Proxmox Backup Server
through CSI snapshots. It ships as a Go binary in a container image plus a
Helm chart.

`docs/design.md` holds the requirements and the design. Read it before
changing behavior, and update it in the same change when the design moves.

## Layout

- `cmd/pbs-volume-backup/`: main package, subcommands `backup`, `restore`.
- `internal/backup/`: driver and restore logic (selection, snapshot, clone,
  pod, cleanup).
- `charts/pbs-volume-backup/`: Helm chart.
- `Dockerfile`: image with the binary and the official
  `proxmox-backup-client-static` package (pinned version and SHA-256).
- `.goreleaser.yaml`: release binaries only; the image comes from the
  Dockerfile.
- `.github/workflows/ci.yaml`: tests, image, end-to-end test, publishing and
  releases on `v*` tags.
- `test/e2e/run.sh`: end-to-end test on kind with a PBS container.
- `spikes/`: the manual tests of milestone M0.

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
