# pbs-volume-backup

Backs up every PersistentVolumeClaim in a Kubernetes cluster to a
[Proxmox Backup Server](https://www.proxmox.com/en/products/proxmox-backup-server/overview).
Each night it snapshots each volume through CSI, backs up the snapshot with
`proxmox-backup-client`, and cleans up. PVCs labelled
`pbs-volume-backup/skip: "true"` are left out.

Status: design phase. See [docs/design.md](docs/design.md).
