# Design

pbs-volume-backup backs up Kubernetes PersistentVolumeClaims to a Proxmox
Backup Server (PBS). It is installed once per cluster as a Helm chart and
backs up every PVC by default. This document holds the requirements and the
intended design. It is the brief for building the first version.

## Context

The first user is a homelab:

- Talos Linux cluster (3 nodes), managed with Flux.
- Volumes come from the Synology CSI driver over iSCSI
  (`csi.san.synology.com`, storage class `synology-iscsi`, ext4,
  `ReadWriteOnce`).
- PBS 4.2 is the central backup target for everything: PCs, the NAS, and
  later the Proxmox guests. A second PBS offsite will pull from it with a sync
  job, so anything that lands in PBS is also offsite.
- The pattern everywhere: the backup runs where the data lives and pushes to
  PBS with `proxmox-backup-client`. For the cluster, that means inside the
  cluster.

Nothing in this repo is specific to that homelab. Values like the PBS
address, datastore and namespace come from Helm values.

## Requirements

### Must

1. **Every PVC is backed up by default.** A PVC is skipped only if it carries
   the opt-out label (default `pbs-volume-backup/skip: "true"`). New apps must
   not need any extra step to be backed up.
2. **Consistent data.** Never read a live volume. Each run takes a CSI
   `VolumeSnapshot`, creates a temporary PVC from it, and backs up that copy.
   The result is crash-consistent, which SQLite and Postgres recover from.
3. **Clean up always.** The snapshot, the temporary PVC and the backup pod are
   deleted after each volume, also when a step fails. All objects the tool
   creates carry a label, so the next run also deletes leftovers from a run
   that crashed.
4. **One PBS backup group per PVC.** Backup type `host`, backup ID
   `<namespace>.<pvc>`. A Kubernetes namespace can't contain a dot, so the ID
   is unambiguous. All groups go into one PBS namespace, set in the values.
5. **Archives per snapshot:**
   - `volume.pxar`: the volume's files. Use `--change-detection-mode
     metadata`, so unchanged files are not read again.
   - `pvc.conf`: the PVC (and its PV) as YAML, for restores.
6. **Restore is part of the tool**, not only a document. A `restore`
   subcommand creates a PVC from a PBS snapshot: it creates the PVC, mounts it
   in a pod, and runs `proxmox-backup-client restore`. The README documents
   both restoring in place (app scaled to zero) and into a new PVC.
7. **Credentials only from the cluster.** The PBS API token secret comes from
   a Kubernetes Secret, referenced by name in the values. The PBS TLS
   fingerprint is pinned in the values. Nothing secret is ever logged.
8. **Failure is visible.** The CronJob's Job fails when any volume failed.
   The log has one line per volume with its result. Each failure is also
   recorded as a Kubernetes Event on the PVC. Alerting is out of scope
   (the cluster will have general notifications later).
9. **Runs under Pod Security "baseline".** Talos enforces baseline by
   default. The backup pod must read files of any owner, so it runs as root
   with the read-only volume mount. `DAC_READ_SEARCH` is not allowed under
   baseline; `DAC_OVERRIDE` is, and it is in containerd's default capability
   set. Drop all other capabilities.
10. **Least privilege.** The driver's ServiceAccount may list PVCs and PVs,
    and create, get and delete VolumeSnapshots, PVCs and Pods, and create
    Events. It may not read Secrets outside its own namespace. See the open
    question on how the backup pod gets the token in other namespaces.

### Should

- Back up volumes one at a time by default, with a configurable concurrency.
- Map storage classes to `VolumeSnapshotClass`es through values; use the
  default class (annotation
  `snapshot.storage.kubernetes.io/is-default-class`) when no mapping is set.
- Pass extra client options through values (for example `--crypt-mode` and a
  key file, so encryption can be turned on later without code changes).
- A `--dry-run` that lists what would be backed up.

### Out of scope

- etcd and cluster objects. Flux restores those from Git.
- Pruning, verification and garbage collection. PBS does these with its own
  jobs per namespace.
- Application-aware hooks (database dumps, fsfreeze). Maybe later, as an
  annotation on the PVC.

## Design

### Parts

- **Go binary** with two subcommands:
  - `backup`: the driver. It lists PVCs, then for each one creates the
    snapshot, waits until it is ready, creates the temporary PVC and a backup
    pod in the PVC's namespace, waits for the pod, records the result and
    cleans up.
  - `restore`: see requirement 6.
  - The backup pod runs the same image with a small `run-client`
    subcommand (or a shell entrypoint) that calls `proxmox-backup-client`.
  - Uses client-go. No kubectl in the image.
- **Container image**: the Go binary plus the official
  `proxmox-backup-client-static` package from the Proxmox repository
  (pinned version and SHA-256, unpacked with `dpkg-deb`). amd64 only, because
  Proxmox publishes the static client only for amd64.
- **Helm chart**: CronJob (driver), ServiceAccount, ClusterRole and binding,
  values for the PBS repository, fingerprint, namespace, token Secret,
  schedule, opt-out label, concurrency and snapshot class mapping.

### Flow per PVC

1. Create a `VolumeSnapshot` of the PVC; wait for `readyToUse`.
2. Create a PVC with `dataSource` set to the snapshot, the same storage class
   and size. With `WaitForFirstConsumer` it binds when the pod starts.
3. Start a pod that mounts that PVC read-only and runs
   `proxmox-backup-client backup volume.pxar:/data pvc.conf:/meta/pvc.yaml
   --ns <ns> --backup-type host --backup-id <namespace>.<pvc>
   --change-detection-mode metadata`.
4. Wait for the pod to finish; read its exit code and log.
5. Delete pod, temporary PVC and snapshot. With the reclaim policy `Retain`
   on the storage class, the temporary PV stays behind: the driver must
   delete the PV (and through the CSI driver the volume) too. Verify this on
   Synology.

### Things to verify early

- The Synology CSI driver can create a PVC from a `VolumeSnapshot`
  (`dataSource`), and how long a clone of a 20 GiB LUN takes.
- Where the backup pod must run: an iSCSI clone can attach to any node, so
  there is no node affinity to the app. Check that this holds.
- `--change-detection-mode metadata` needs the previous snapshot's metadata
  index. Check that it works when the source path is a new clone each run
  (inode numbers change, so the client may read everything again).

## Testing

- Unit tests for PVC selection, object naming, and cleanup after each failing
  step (fake clientset).
- End-to-end in CI: kind with the CSI hostpath driver (it supports
  snapshots and clones) and a PBS instance in a container. Back up a PVC,
  restore it into a new PVC, compare the files.

## Release

- GoReleaser builds the binary and the image on a tag and pushes the image to
  `ghcr.io/prioe/pbs-volume-backup`.
- The same workflow pushes the chart as an OCI artifact to
  `oci://ghcr.io/prioe/charts/pbs-volume-backup`, with chart version and
  `appVersion` equal to the tag.
- The repo and both packages are public, so clusters need no pull secret.

## Cluster prerequisites (done in the cluster's own repo)

- VolumeSnapshot CRDs and the snapshot-controller.
- A `VolumeSnapshotClass` for each CSI driver in use.
- On PBS: a namespace for the cluster, a prune job for it, and a user with an
  API token that has `DatastoreBackup` on that namespace only.
- A Secret with the token, and a Flux `OCIRepository` plus `HelmRelease`.

## Open questions

- How the backup pod gets the PBS token in the PVC's namespace. Options: the
  driver copies the Secret into the namespace for the run and deletes it
  afterwards (needs create/delete on Secrets there), or the driver streams
  the token to the pod some other way. Decide before building.
- License.
- Which PBS container image to use in CI (Proxmox publishes none).
