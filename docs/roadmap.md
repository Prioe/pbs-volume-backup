# Roadmap

This file orders the work to build the first version described in
[design.md](design.md). The design holds the requirements and the reasons;
this file only says what to build in which order and when a step is done.
"R1" to "R10" refer to the Must requirements in the design.

## M0: Decisions and spikes

Settle what can change the design before writing code that depends on it.
Each spike ends with a note in design.md: the result, and a design change if
one is needed.

### Decisions

1. **Token delivery (open question).** This decides where the backup pod
   runs, the RBAC and most of M2. Options:
   - **A. Copy the Secret** into the PVC's namespace for the run, labelled,
     deleted in cleanup. The driver needs `create` and `delete` on Secrets in
     all namespaces, but no `get`, `list` or `watch`, so it still can't read
     them (R10). While the copy exists, anyone who can create pods in that
     namespace can mount it.
   - **B. Stream the token** into the pod over `pods/attach` stdin. Nothing
     is stored outside the tool's namespace. More moving parts, harder to
     debug.
   - **C. Run every backup pod in the tool's namespace.** A clone PVC must be
     in the same namespace as its VolumeSnapshot, and that snapshot must be
     in the source PVC's namespace. But the driver can import the snapshot
     into its own namespace: a static `VolumeSnapshotContent` with the same
     `snapshotHandle` and `deletionPolicy: Retain`, plus a VolumeSnapshot
     bound to it. The clone, the pod and the Secret then stay in one
     namespace. This also avoids app namespaces whose Pod Security level is
     stricter than baseline, where a root pod is rejected (R9). Needs
     `create`, `get` and `delete` on VolumeSnapshotContents, and the Synology
     CSI driver must accept an imported handle (spike 4).

   Decided: C. Spike 4 passed; design.md describes the flow.

2. **License.** Decided: AGPL-3.0-or-later, the same as
   `proxmox-backup-client`.

3. **PBS in CI (open question).** Proxmox publishes no container image.
   Research the options (a community image, or one built from the Proxmox
   apt repository in this repo) and pick one. Only M6 depends on it.

### Spikes on the homelab cluster

Run these by hand before M2. The manifests and steps are in
[spikes/](../spikes/README.md). All five ran on 2026-11-03; the results are
in design.md.

1. **Clone from snapshot.** Snapshot a 20 GiB Synology PVC, create a PVC
   with `dataSource` pointing to it, and time how long it takes until a pod
   can mount the clone.
2. **Reclaim policy.** Check what happens to the LUN when the clone PVC and
   its PV are deleted. With `Retain`, deleting the PV object most likely
   leaves the LUN on the NAS. If so, the driver patches the clone PV's
   `persistentVolumeReclaimPolicy` to `Delete` before deleting the clone PVC,
   which needs `patch` on PVs. Do the same check for the VolumeSnapshotClass
   `deletionPolicy`.
3. **Node placement and read-only mount.** Start the pod on a node other
   than the app's. Mount the clone with `readOnly: true` in the pod spec, not
   with a `ReadOnlyMany` access mode: a crash-consistent ext4 clone may need
   a journal replay, which needs a writable device at stage time.
4. **Snapshot import (only for decision 1C).** Create a static
   VolumeSnapshotContent in the tool's namespace from the handle of spike 1,
   clone from it, then delete the import and check that the original
   snapshot is still there.
5. **Metadata change detection.** Back up the same PVC twice from two fresh
   clones with `--change-detection-mode metadata` and compare the amount of
   data read. A block-level clone keeps ext4 inode numbers, so it may work;
   the CSI hostpath driver used in CI copies files and will not. If every run
   reads everything, soften R5 in design.md.

Done when: every decision is written into design.md, the open questions
section is empty, and each spike has a recorded result.

## M1: Skeleton

- Go module, `cmd/pbs-volume-backup/` with the subcommands `backup` and
  `restore` as stubs. `internal/` follows in M2, with the
  first code that belongs there.
- Dockerfile: the Go binary plus `proxmox-backup-client-static`, unpacked
  with `dpkg-deb`. Look up the current package version and its SHA-256 in
  the Proxmox apt repository and pin both. amd64 only.
- CI: build, `go vet`, golangci-lint, unit tests, image build without push.
- LICENSE file.

Done when: CI is green and `docker run <image> proxmox-backup-client
version` works.

## M2: Backup driver

The driver logic, tested against the fake clientset. Closes R1, R2, R3, R4,
R8, R9.

- **Selection:** list all PVCs, skip the opt-out label, skip PVCs that are
  not `Bound`.
- **Naming:** object names derived from namespace and PVC, kept within the
  63-character limit; backup ID `<namespace>.<pvc>`.
- **Snapshot:** class from the storage class mapping or the default class;
  wait for `readyToUse` with a timeout.
- **Clone:** PVC from the snapshot, same storage class, size from the
  snapshot's `restoreSize`.
- **Pod:** root, only `DAC_OVERRIDE`, read-only mount, fits Pod Security
  baseline. The command line carries no secret.
- **Result:** wait for the pod, read exit code and log, one log line per
  volume, an Event on the PVC on failure, non-zero exit if any volume failed.
- **Cleanup:** delete pod, clone, PV handling from spike 2, and snapshot on
  every exit path, also on SIGTERM. At start, delete leftovers found by the
  `app.kubernetes.io/managed-by` label.
- **Unit tests:** selection, naming, and cleanup after a failure at each
  step.

Done when: unit tests pass and a manual `backup` run against the homelab
backs up one PVC end to end.

## M3: Client and options

Closes R5 and R7, and the Should items.

- Done early in M2: the client call, the token from the Secret in the
  tool's namespace, and `pvc.conf` from a ConfigMap. `run-client` was
  dropped: the pod calls `proxmox-backup-client` directly.
- Extra client options passed through (for example `--crypt-mode` and a key
  file).
- `--dry-run`: print the PVCs that would be backed up and the snapshot class
  each would use.
- Concurrency: a worker pool, default 1.

Done when: the homelab has a backup with both archives in PBS and a second
run shows the effect of the change detection mode measured in spike 5.

## M4: Restore

Closes R6.

- `restore` takes a PBS snapshot (or "latest" of a group), a target
  namespace and PVC name, and optionally a storage class and size; by
  default it reads them from `pvc.conf`.
- It creates the PVC, a pod that mounts it read-write, runs
  `proxmox-backup-client restore`, and cleans up the pod and any temporary
  Secret. The PVC stays.
- `restore` runs with the operator's kubeconfig, not the driver's
  ServiceAccount.
- README: restore in place (app scaled to zero, PVC deleted and recreated)
  and restore into a new PVC.

Done when: a homelab PVC restored into a new PVC has the same files as the
source.

## M5: Helm chart

- CronJob, ServiceAccount, ClusterRole and binding, with exactly the rights
  that M0 to M4 turned out to need. Check that `pods/log` (for the result)
  and the PV patch from spike 2 are in, and nothing else.
- Values, documented in `values.yaml`: PBS repository, fingerprint,
  namespace, token Secret, schedule, opt-out label, concurrency, snapshot
  class mapping, extra client options, image.
- `concurrencyPolicy: Forbid` and a CronJob deadline.
- `helm lint` and `helm template` in CI.

Done when: the homelab runs the chart through Flux and the nightly job
succeeds.

## M6: End-to-end test in CI

- kind with the CSI hostpath driver, the snapshot CRDs and the
  snapshot-controller, and PBS from decision 3.
- Install the chart, run the backup job, restore into a new PVC, compare the
  files.
- Failure cases: a PVC with the opt-out label is skipped; a forced failure
  leaves no objects behind and fails the job.

Done when: the e2e job runs on every pull request and passes.

## M7: Release

- GoReleaser builds the binary and the image and pushes the image to
  `ghcr.io/prioe/pbs-volume-backup` on a tag.
- The same workflow pushes the chart to
  `oci://ghcr.io/prioe/charts/pbs-volume-backup`, with version and
  `appVersion` equal to the tag.
- Tag v0.1.0 and switch the homelab to the released chart.

Done when: a fresh cluster can install the chart from the OCI registry
without a pull secret.
