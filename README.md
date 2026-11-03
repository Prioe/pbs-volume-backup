# pbs-volume-backup

Backs up every PersistentVolumeClaim in a Kubernetes cluster to a
[Proxmox Backup Server](https://www.proxmox.com/en/products/proxmox-backup-server/overview).
Each night it snapshots each volume through CSI, backs up the snapshot with
`proxmox-backup-client`, and cleans up. PVCs labelled
`pbs-volume-backup/skip: "true"` are left out.

Status: in development. See [docs/design.md](docs/design.md) and
[docs/roadmap.md](docs/roadmap.md).

## License

pbs-volume-backup is licensed under the
[GNU Affero General Public License v3.0 or later](LICENSE).

The container image also contains `proxmox-backup-client` from the
`proxmox-backup-client-static` package by Proxmox Server Solutions GmbH,
unmodified and also licensed under AGPL-3.0-or-later. Its license is in the
image at `/usr/share/doc/proxmox-backup-client-static/copyright`. The source
is in the [proxmox-backup repository](https://git.proxmox.com/?p=proxmox-backup.git),
at the tag of the client version. The image labels
`one.prio.pbs-client.version` and `one.prio.pbs-client.source` name the
version and link to the tag.
