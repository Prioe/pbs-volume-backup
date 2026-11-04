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
   set. Drop all other capabilities. The backup pod runs in the tool's own
   namespace (see the flow below), so app namespaces may enforce a stricter
   level.
10. **Least privilege.** The driver's ServiceAccount may:
    - list PVCs, and list, create, get and delete VolumeSnapshots, in all
      namespaces;
    - list, create, get and delete VolumeSnapshotContents;
    - list VolumeSnapshotClasses;
    - list, get, patch and delete PVs;
    - list, create, get and delete PVCs, Pods and ConfigMaps, and get
      `pods/log`, in its own namespace;
    - create Events.

    It reads no Secrets: the backup pod mounts the token Secret from the
    tool's own namespace. RBAC can't limit PV and VolumeSnapshotContent
    access to the tool's own objects; the driver only touches the ones it
    created.

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
    snapshot, waits until it is ready, imports it into its own namespace,
    creates the temporary PVC and a backup pod there, waits for the pod,
    records the result and cleans up.
  - `restore`: see requirement 6.
  - The backup pod runs the same image and calls `proxmox-backup-client`
    directly. The image is distroless and has no shell.
  - Uses client-go. No kubectl in the image.
- **Container image**: the Go binary plus the official
  `proxmox-backup-client-static` package from the Proxmox repository
  (pinned version and SHA-256, unpacked with `dpkg-deb`). amd64 only, because
  Proxmox publishes the static client only for amd64.
- **Helm chart**: CronJob (driver), ServiceAccount, ClusterRole and binding,
  values for the PBS repository, fingerprint, namespace, token Secret,
  schedule, opt-out label, concurrency and snapshot class mapping.

### Flow per PVC

1. Create a `VolumeSnapshot` of the PVC in the PVC's namespace; wait for
   `readyToUse`.
2. Import the snapshot into the tool's namespace. A clone PVC must be in the
   same namespace as its VolumeSnapshot, so the driver creates a
   `VolumeSnapshotContent` with the same `snapshotHandle`,
   `sourceVolumeMode: Filesystem` and `deletionPolicy: Retain`, and a
   VolumeSnapshot in its own namespace bound to it. This copies no data.
3. Create a PVC in the tool's namespace with `dataSource` set to the
   imported snapshot, the same storage class, and the size from the
   snapshot's `restoreSize`. With `WaitForFirstConsumer` it binds when the
   pod starts.
4. Write the PVC and its PV as YAML into a ConfigMap. Start a pod in the
   tool's namespace that mounts the clone with `readOnly: true` in the pod
   spec (not a `ReadOnlyMany` access mode: a crash-consistent ext4 may need
   a journal replay when the volume is staged), the ConfigMap and the token
   Secret. It runs `proxmox-backup-client backup volume.pxar:/data
   pvc.conf:/meta/pvc.yaml --ns <ns> --backup-type host --backup-id
   <namespace>.<pvc> --change-detection-mode metadata`.
5. Wait for the pod to finish; read its exit code and log.
6. Clean up:
   - Delete the pod and the ConfigMap.
   - Set the clone PV's `persistentVolumeReclaimPolicy` to `Delete`, then
     delete the clone PVC. The PV and the volume on the storage go with it.
     Deleting a `Retain` PV instead leaves the volume on the storage.
   - Delete the imported VolumeSnapshot and its VolumeSnapshotContent. With
     `Retain`, the snapshot on the storage stays.
   - Delete the original VolumeSnapshot. The VolumeSnapshotClass must have
     `deletionPolicy: Delete`, or every run leaves a snapshot on the
     storage.

The backup pod needs no node affinity: the clone attaches to any node.

The tool's namespace is reserved for the tool: PVCs in it are not backed up.
Before the first PVC, each run deletes all objects with the managed-by label
that a crashed run left behind, in the same order as above. It also sets
released PVs of clones in its namespace to `Delete`; such a PV is left when
a clone PVC is deleted while its volume is still being provisioned.

A PVC that can't be backed up (no CSI volume, a block volume, or no
VolumeSnapshotClass for its driver) counts as failed, not skipped. The
error names the opt-out label.

### Restore

`restore` runs on the operator's machine with the operator's kubeconfig, not
with the driver's ServiceAccount.

1. Check the target. A new PVC must not exist yet; with `--into-existing`
   it must exist and no running pod may mount it.
2. Delete pods and the Secret of an interrupted restore of the same target,
   and token copies of killed restores in all namespaces. A copy whose
   restore pod is still pending or running belongs to a concurrent restore
   and stays.
3. Without `--snapshot`, run `snapshot list` in a pod in the tool's
   namespace and pin the latest snapshot, so `pvc.conf` and the volume come
   from the same one.
4. Run `restore <snapshot> pvc.conf -` in a second pod there and read the
   PVC from its log.
5. Create the target PVC from it, unless it exists: storage class, size,
   access modes and labels, with overrides from the flags.
6. Copy the token Secret into the target namespace and start the restore
   pod there. It mounts the PVC read-write and runs `restore <snapshot>
   volume.pxar /data --allow-existing-dirs true` (a new ext4 volume already
   has `lost+found`), plus `--overwrite true` for `--into-existing`. It runs
   as root with `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `MKNOD` and
   `SETFCAP`, all allowed under baseline.
7. Delete the pods and the Secret copy. If the restore failed, also delete
   a PVC that step 5 created, with its PV set to `Delete`.

### Spike results

Measured on 2026-11-03 on the first user's cluster (Synology CSI, LUNs on
Btrfs) with the manifests in [spikes/](../spikes/README.md):

- A snapshot of a 20 GiB LUN is ready in 2 s. A clone from it is mounted
  in a pod on another node within 9 to 17 s. Both are copy-on-write on the
  NAS.
- The clone mounts with `readOnly: true`. Root with only `DAC_OVERRIDE`
  reads a directory owned by UID 1000 with mode 0700. Without the
  capability it gets "Permission denied".
- Importing the snapshot handle into another namespace works (the driver
  supports `LIST_SNAPSHOTS`). The import is ready in 1 s. Deleting it leaves
  the original snapshot on the NAS.
- `--change-detection-mode metadata` works on a fresh clone each run. A
  block-level clone keeps the inode numbers. The second run reused 4.04 GiB
  (100%), uploaded 54 KiB and took 1 s, against 202 s for the first run.
  The CSI hostpath driver in CI copies files instead, so it can't show this.
- With reclaim policy `Retain`, deleting the PV left the LUN on the NAS.
  Setting the PV to `Delete` first removed it. A VolumeSnapshotClass with
  `deletionPolicy: Delete` removed the snapshots on the NAS.
- Crash consistency: data that the app had appended but the kernel had not
  yet written out shows up as NUL bytes at the end of the file in the clone.
  Apps that call `fsync` (SQLite, Postgres) are not affected. The README
  should say this.
- `csi-snapshotter` older than v6 fails with snapshot-controller v8:
  "sourceVolumeMode is required once set".

## Testing

- Unit tests for PVC selection, object naming, and cleanup after each failing
  step (fake clientset).
- End-to-end in CI: kind with the CSI hostpath driver (it supports
  snapshots and clones) and a PBS instance in a container. Back up a PVC,
  restore it into a new PVC, compare the files.

## Release

- GoReleaser builds the binary and the image on a tag and pushes the image to
  `ghcr.io/prioe/pbs-volume-backup`.
- Before that, CI pushes a development image on every push to `main`, after
  the tests pass, tagged `main` and `sha-<commit>`.
- The same workflow pushes the chart as an OCI artifact to
  `oci://ghcr.io/prioe/charts/pbs-volume-backup`, with chart version and
  `appVersion` equal to the tag.
- The repo and both packages are public, so clusters need no pull secret.

## Cluster prerequisites (done in the cluster's own repo)

- VolumeSnapshot CRDs and the snapshot-controller, and a `csi-snapshotter`
  sidecar for each CSI driver at a version that matches the controller.
- A `VolumeSnapshotClass` for each CSI driver in use, with
  `deletionPolicy: Delete`.
- On PBS: a namespace for the cluster, a prune job for it, and a user with an
  API token that has `DatastoreBackup` on that namespace only.
- A Secret with the token, and a Flux `OCIRepository` plus `HelmRelease`.

## Open questions

- Which PBS container image to use in CI (Proxmox publishes none).
