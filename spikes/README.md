# Spikes

Manual tests for milestone M0 of the [roadmap](../docs/roadmap.md). They run
against a real cluster with the Synology CSI driver (storage class
`synology-iscsi`) and the snapshot CRDs and controller installed. Write each
result into [design.md](../docs/design.md).

The spikes share objects, so run them in this order: 1, 3, 4, 5, 2. Spike 2
is also the cleanup.

Templated files use `envsubst` (from gettext). Always pass the variable list,
as in `envsubst '$RUN'`, so shell variables in the manifests stay as they
are.

Requirements:

- A default VolumeSnapshotClass for `csi.san.synology.com` (annotation
  `snapshot.storage.kubernetes.io/is-default-class: "true"`).
- At least two nodes. The reader pods refuse to run on the writer's node.
- Spike 5 only: a PBS namespace `spike` and an API token with
  `DatastoreBackup` on it.

## Setup

```sh
cd spikes
kubectl apply -f 00-namespaces.yaml
kubectl apply -f 01-source.yaml
until kubectl -n pbs-spike exec spike-writer -- test -f /data/.seeded; do sleep 10; done
```

The writer fills the 20 GiB volume with 10,000 small files (owned by UID
1000, mode 0700 on the directory) and one 4 GiB file. Then it appends a line
to `/data/live.log` every second, so snapshots are taken of a volume in use.

## Spike 1: clone from snapshot

```sh
export RUN=1
envsubst '$RUN' < 02-snapshot.yaml | kubectl apply -f -
time kubectl -n pbs-spike wait volumesnapshot/spike-snap-1 \
  --for=jsonpath='{.status.readyToUse}'=true --timeout=10m
time (envsubst '$RUN' < 03-clone.yaml | kubectl apply -f - &&
  kubectl -n pbs-spike wait pod/spike-reader-1 --for=condition=Ready --timeout=30m)
kubectl -n pbs-spike logs spike-reader-1
```

Record: time until the snapshot is ready, time until the reader runs.

Passes when the reader prints 10000, the inode numbers, an MD5 sum and a
date from `live.log`.

## Spike 3: node placement, read-only mount, capabilities

Spike 1 already covers this. Check:

```sh
kubectl -n pbs-spike get pod spike-writer spike-reader-1 -o wide
```

- The two pods are on different nodes.
- The reader mounted the clone with `readOnly: true`, and the
  crash-consistent ext4 did mount. If it hangs in `ContainerCreating`,
  `kubectl -n pbs-spike describe pod spike-reader-1` shows the mount error.
- The reader listed `/data/small` as root with only `DAC_OVERRIDE`. To show
  that the capability is what grants access, delete the reader, change
  `add: [DAC_OVERRIDE]` to `add: []` in a copy of `03-clone.yaml`, apply it,
  and expect `Permission denied`. Restore the original before spike 5.

## Spike 4: import the snapshot into another namespace

This decides option C for the token delivery.

```sh
CONTENT=$(kubectl -n pbs-spike get volumesnapshot spike-snap-1 \
  -o jsonpath='{.status.boundVolumeSnapshotContentName}')
export SNAPSHOT_HANDLE=$(kubectl get volumesnapshotcontent "$CONTENT" \
  -o jsonpath='{.status.snapshotHandle}')
envsubst '$SNAPSHOT_HANDLE' < 04-import.yaml | kubectl apply -f -
kubectl -n pbs-spike-tool wait volumesnapshot/spike-import \
  --for=jsonpath='{.status.readyToUse}'=true --timeout=10m
kubectl -n pbs-spike-tool wait pod/spike-import-reader --for=condition=Ready --timeout=30m
kubectl -n pbs-spike-tool logs spike-import-reader
```

Passes when the output matches spike 1's reader. If the import never gets
ready, `kubectl describe volumesnapshotcontent spike-import` shows why (for
example, the driver does not support listing snapshots).

Then remove the import and check that the original snapshot survived:

```sh
IMPORT_PV=$(kubectl -n pbs-spike-tool get pvc spike-import-clone -o jsonpath='{.spec.volumeName}')
kubectl -n pbs-spike-tool delete pod spike-import-reader
kubectl patch pv "$IMPORT_PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl -n pbs-spike-tool delete pvc spike-import-clone
kubectl -n pbs-spike-tool delete volumesnapshot spike-import
kubectl delete volumesnapshotcontent spike-import
kubectl -n pbs-spike get volumesnapshot spike-snap-1
```

Passes when `spike-snap-1` is still `READYTOUSE true` and the snapshot is
still listed in DSM.

## Spike 5: metadata change detection

Copy `05-pbs.yaml` to `05-pbs.local.yaml` (ignored by Git) and fill in the
repository and fingerprint. Put the token secret into a file outside the
repository; it never goes on a command line.

```sh
kubectl apply -f 05-pbs.local.yaml
kubectl -n pbs-spike create secret generic spike-pbs-token --from-file=password=/path/to/token
kubectl -n pbs-spike delete pod spike-reader-1
export RUN=1
envsubst '$RUN' < 05-backup-job.yaml | kubectl apply -f -
kubectl -n pbs-spike wait job/spike-backup-1 --for=condition=Complete --timeout=2h
kubectl -n pbs-spike logs job/spike-backup-1
```

The reader must be gone first: the clone is `ReadWriteOnce`, and the job pod
may land on another node.

Now take a second snapshot and back up its clone:

```sh
export RUN=2
envsubst '$RUN' < 02-snapshot.yaml | kubectl apply -f -
kubectl -n pbs-spike wait volumesnapshot/spike-snap-2 \
  --for=jsonpath='{.status.readyToUse}'=true --timeout=10m
envsubst '$RUN' < 03-clone.yaml | kubectl apply -f -
kubectl -n pbs-spike wait pod/spike-reader-2 --for=condition=Ready --timeout=30m
kubectl -n pbs-spike logs spike-reader-2
kubectl -n pbs-spike delete pod spike-reader-2
envsubst '$RUN' < 05-backup-job.yaml | kubectl apply -f -
kubectl -n pbs-spike wait job/spike-backup-2 --for=condition=Complete --timeout=2h
kubectl -n pbs-spike logs job/spike-backup-2
```

Record: the duration of both jobs, the summary at the end of both logs (data
read, reused and uploaded), and whether the inode numbers of reader 1 and
reader 2 match.

Passes when run 2 reads far less than the 4 GiB of run 1. If run 2 reads
everything again, soften R5 in design.md.

## Spike 2: reclaim policy (and cleanup)

Check the policies:

```sh
kubectl get storageclass synology-iscsi -o jsonpath='{.reclaimPolicy}{"\n"}'
kubectl get volumesnapshotclass \
  -o custom-columns=NAME:.metadata.name,DRIVER:.driver,POLICY:.deletionPolicy
```

Delete clone 1 as is:

```sh
kubectl -n pbs-spike delete job spike-backup-1 spike-backup-2
PV1=$(kubectl -n pbs-spike get pvc spike-clone-1 -o jsonpath='{.spec.volumeName}')
kubectl -n pbs-spike delete pvc spike-clone-1
kubectl get pv "$PV1"
kubectl delete pv "$PV1"
```

With `Retain`, the PV stays as `Released` until it is deleted. Then look in
DSM (SAN Manager) for the LUN of `$PV1`. If it is still there, deleting a PV
leaks the LUN.

Delete clone 2 after switching its PV to `Delete`:

```sh
PV2=$(kubectl -n pbs-spike get pvc spike-clone-2 -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$PV2" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl -n pbs-spike delete pvc spike-clone-2
kubectl get pv "$PV2"
```

Passes when the PV disappears by itself and its LUN is gone from DSM.

Delete the snapshots and check that their VolumeSnapshotContents and the
snapshots in DSM are gone too:

```sh
kubectl -n pbs-spike delete volumesnapshot spike-snap-1 spike-snap-2
kubectl get volumesnapshotcontent
```

If a policy is `Retain`, delete what is left by hand in DSM.

Finally, remove the source:

```sh
SRC_PV=$(kubectl -n pbs-spike get pvc spike-source -o jsonpath='{.spec.volumeName}')
kubectl -n pbs-spike delete pod spike-writer
kubectl patch pv "$SRC_PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl delete -f 00-namespaces.yaml
```

Also delete the backup group `host/pbs-spike.spike-source` and the namespace
`spike` on PBS.
