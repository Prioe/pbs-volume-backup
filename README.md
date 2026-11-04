# pbs-volume-backup

Backs up every PersistentVolumeClaim in a Kubernetes cluster to a
[Proxmox Backup Server](https://www.proxmox.com/en/products/proxmox-backup-server/overview).
Each night it snapshots each volume through CSI, backs up the snapshot with
`proxmox-backup-client`, and cleans up. PVCs labelled
`pbs-volume-backup/skip: "true"` are left out.

Status: in development. See [docs/design.md](docs/design.md) and
[docs/roadmap.md](docs/roadmap.md).

Backups are crash-consistent: they contain what a sudden power loss would
have left on disk. Apps that call `fsync` for their data, such as SQLite and
Postgres, recover from that. Data an app had written but the kernel had not
yet flushed can show up as zero bytes at the end of a file.

## Restore

`pbs-volume-backup restore` restores a backup of a PVC. It runs on your
machine with your kubeconfig and needs the same PBS settings as the backup:
the tool's namespace with the token Secret, the image, the repository, the
fingerprint and the PBS namespace. The flags also read the environment
variables of the backup (`pbs-volume-backup restore -h` lists them).

It restores the latest snapshot unless you pass `--snapshot` with a backup
time such as `2026-11-04T18:04:09Z`. The new PVC gets the storage class, size,
access modes and labels of the original, read from the `pvc.conf` archive;
`--storage-class` and `--size` override them. If the restore fails, the tool
deletes the PVC it created.

The restore pod runs in the target namespace as root with the capabilities
that restoring owners and modes needs. The namespace must allow Pod Security
`baseline`. The tool copies the token Secret into the namespace for the
restore and deletes the copy afterwards.

### Into a new PVC

Restore next to the original, for example to compare or copy files:

```sh
pbs-volume-backup restore --from app/data --to app/data-restored
```

### In place

1. Stop whatever reconciles the app, so it doesn't scale it back up or
   recreate the PVC, for example `flux suspend helmrelease app -n app`.
2. Scale the app to zero, for example
   `kubectl -n app scale statefulset app --replicas 0`.
3. Delete the PVC: `kubectl -n app delete pvc data`. With reclaim policy
   `Retain`, the old volume stays on the storage until you delete its PV.
4. Restore: `pbs-volume-backup restore --from app/data`.
5. Scale the app up and resume the reconciler.

If something recreates the PVC before step 4, restore into it instead:
`pbs-volume-backup restore --from app/data --into-existing`. Files from the
backup overwrite files of the same name; other files in the PVC stay.

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
