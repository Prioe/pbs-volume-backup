#!/usr/bin/env bash
# End-to-end test: kind with the CSI hostpath driver and a PBS container.
# Needs docker, kind, kubectl, helm, go and git. KEEP=1 keeps the cluster.
# E2E_AFTER_CLUSTER names a script that runs right after the cluster exists,
# with CLUSTER, WORK and KUBECONFIG set.
set -euo pipefail

CLUSTER=pvb-e2e
NODE_IMAGE=kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
PBS_IMAGE=ayufan/proxmox-backup-server:v4.2.0-1@sha256:01f685a010025233b649417a317cc175b27920cd64f0429abfdc46a0d8d2e88a
SNAPSHOTTER_VERSION=v8.7.0
HOSTPATH_VERSION=v1.19.0
IMAGE=pbs-volume-backup:e2e
PBS=pvb-e2e-pbs
TOOL_NS=pbs-volume-backup
PBS_NS=k8s

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
export KUBECONFIG=$WORK/kubeconfig

log() { printf '\n=== %s\n' "$*"; }
fail() {
	printf '\nFAIL: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	status=$?
	if [[ $status -ne 0 ]]; then
		log "diagnostics"
		kubectl get pods,pvc,jobs -A -o wide || true
		kubectl get volumesnapshots,volumesnapshotcontents -A || true
		kubectl -n "$TOOL_NS" logs -l job-name --tail=50 --prefix || true
	fi
	if [[ ${KEEP:-} == 1 ]]; then
		echo "KEEP=1: cluster $CLUSTER and container $PBS stay; KUBECONFIG=$KUBECONFIG"
	else
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
		docker rm -f "$PBS" >/dev/null 2>&1 || true
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT

# client runs proxmox-backup-client from the test image against the PBS.
client() {
	local auth=$1 password=$2
	shift 2
	docker run --rm --network kind -e PBS_FINGERPRINT="$FINGERPRINT" \
		-e PBS_REPOSITORY="$auth@$PBS_IP:e2e" -e PBS_PASSWORD="$password" \
		--entrypoint /usr/bin/proxmox-backup-client "$IMAGE" "$@"
}

wait_job() {
	local ns=$1 job=$2
	for _ in $(seq 300); do
		if [[ $(kubectl -n "$ns" get job "$job" -o jsonpath='{.status.succeeded}') == 1 ]]; then
			return 0
		fi
		if [[ $(kubectl -n "$ns" get job "$job" -o jsonpath='{.status.failed}') == 1 ]]; then
			kubectl -n "$ns" logs "job/$job" || true
			fail "job $ns/$job failed"
		fi
		sleep 2
	done
	fail "job $ns/$job did not finish"
}

# manifest prints names, owners, modes, types and checksums below /data of
# a pod, between the markers that the pod writes.
manifest() {
	local ns=$1 pod=$2
	for _ in $(seq 120); do
		if kubectl -n "$ns" logs "$pod" 2>/dev/null | grep -q '^END$'; then
			kubectl -n "$ns" logs "$pod" | sed -n '/^BEGIN$/,/^END$/p'
			return 0
		fi
		sleep 1
	done
	fail "pod $ns/$pod wrote no manifest"
}

# shellcheck disable=SC2016
MANIFEST_SCRIPT='cd /data
echo BEGIN
find . -mindepth 1 | sort | while read -r f; do stat -c "%n %u:%g %a %F" "$f"; done
find . -type f | sort | xargs md5sum
echo END
'

log "build image"
docker build -q -t "$IMAGE" "$ROOT"
go build -o "$WORK/pvb" "$ROOT/cmd/pbs-volume-backup"

git clone -q --depth 1 --branch "$HOSTPATH_VERSION" https://github.com/kubernetes-csi/csi-driver-host-path "$WORK/hostpath"
CONTROLLER_URL="https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=$SNAPSHOTTER_VERSION"
kubectl kustomize "$CONTROLLER_URL" >"$WORK/snapshot-controller.yaml"

log "kind cluster"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --kubeconfig "$KUBECONFIG" --wait 120s
kind load docker-image "$IMAGE" --name "$CLUSTER"
if [[ -n ${E2E_AFTER_CLUSTER:-} ]]; then
	log "hook $E2E_AFTER_CLUSTER"
	CLUSTER=$CLUSTER WORK=$WORK "$E2E_AFTER_CLUSTER"
fi

log "snapshot CRDs and controller $SNAPSHOTTER_VERSION"
kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=$SNAPSHOTTER_VERSION"
kubectl apply -f "$WORK/snapshot-controller.yaml"
kubectl -n kube-system rollout status deployment/snapshot-controller --timeout=300s

log "CSI hostpath driver $HOSTPATH_VERSION"
"$WORK/hostpath/deploy/kubernetes-1.36/deploy.sh" >"$WORK/hostpath.log" 2>&1 || {
	cat "$WORK/hostpath.log"
	fail "hostpath deploy"
}
kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hostpath
provisioner: hostpath.csi.k8s.io
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: hostpath
  annotations:
    snapshot.storage.kubernetes.io/is-default-class: "true"
driver: hostpath.csi.k8s.io
deletionPolicy: Delete
EOF

log "PBS"
docker run -d --name "$PBS" --network kind --tmpfs /run "$PBS_IMAGE" >/dev/null
for _ in $(seq 60); do
	docker exec "$PBS" test -f /etc/proxmox-backup/authkey.key && break
	sleep 2
done
sleep 3
PBS_IP=$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$PBS")
FINGERPRINT=$(docker exec "$PBS" proxmox-backup-manager cert info | sed -n 's/^Fingerprint (sha256): //p')
token() { docker exec "$PBS" proxmox-backup-manager user generate-token "$1" "$2" | sed -n 's/.*"value": "\(.*\)".*/\1/p'; }
docker exec "$PBS" sh -c '
mkdir -p /tmp/ds
proxmox-backup-manager datastore create e2e /tmp/ds
proxmox-backup-manager user create setup@pbs
proxmox-backup-manager user create ci@pbs
proxmox-backup-manager acl update /datastore/e2e DatastoreAdmin --auth-id setup@pbs' >/dev/null
SETUP_TOKEN=$(token setup@pbs t)
CI_TOKEN=$(token ci@pbs backup)
docker exec "$PBS" proxmox-backup-manager acl update /datastore/e2e DatastoreAdmin --auth-id 'setup@pbs!t'
client 'setup@pbs!t' "$SETUP_TOKEN" namespace create "$PBS_NS"
for id in ci@pbs 'ci@pbs!backup'; do
	docker exec "$PBS" proxmox-backup-manager acl update "/datastore/e2e/$PBS_NS" DatastoreBackup --auth-id "$id"
done

log "workload"
kubectl create namespace app
kubectl apply -n app -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  storageClassName: hostpath
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: skipped
  labels:
    pbs-volume-backup/skip: "true"
spec:
  storageClassName: hostpath
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
spec:
  containers:
    - name: writer
      image: busybox:1.37
      command:
        - sh
        - -c
        - |
          set -e
          mkdir -p /data/private /data/sub/deeper
          echo secret > /data/private/token
          chown -R 1000:1000 /data/private
          chmod 0700 /data/private
          chmod 0640 /data/private/token
          head -c 5000000 /dev/urandom > /data/sub/deeper/big.bin
          echo hello > /data/hello.txt
          chmod 0600 /data/hello.txt
          ln -s sub/deeper/big.bin /data/link
          sync
          $(printf '%s' "$MANIFEST_SCRIPT" | sed '2,$s/^/          /')
          sleep 3600
      volumeMounts:
        - name: data
          mountPath: /data
        - name: skipped
          mountPath: /skipped
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
    - name: skipped
      persistentVolumeClaim:
        claimName: skipped
EOF
manifest app writer >"$WORK/original.txt"
cat "$WORK/original.txt"

log "install chart"
kubectl create namespace "$TOOL_NS"
kubectl label namespace "$TOOL_NS" pod-security.kubernetes.io/enforce=baseline
printf '%s' "$CI_TOKEN" >"$WORK/token"
kubectl -n "$TOOL_NS" create secret generic pbs-token --from-file=password="$WORK/token"
helm install pvb "$ROOT/charts/pbs-volume-backup" -n "$TOOL_NS" --wait \
	--set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" --set image.pullPolicy=Never \
	--set pbs.repository="ci@pbs!backup@$PBS_IP:e2e" --set pbs.fingerprint="$FINGERPRINT" \
	--set pbs.namespace="$PBS_NS" --set pbs.tokenSecret.name=pbs-token \
	--set schedule="0 0 1 1 *"
CRONJOB=$(kubectl -n "$TOOL_NS" get cronjob -o jsonpath='{.items[0].metadata.name}')

for run in 1 2; do
	log "backup run $run"
	kubectl -n "$TOOL_NS" create job --from="cronjob/$CRONJOB" "backup-$run"
	wait_job "$TOOL_NS" "backup-$run"
	kubectl -n "$TOOL_NS" logs "job/backup-$run" | tee "$WORK/backup-$run.log"
	grep -q 'pvc=app/data result=ok' "$WORK/backup-$run.log" || fail "app/data not backed up"
	grep -q 'pvc=app/skipped result=skipped' "$WORK/backup-$run.log" || fail "app/skipped not skipped"
done

log "snapshots in PBS"
client 'ci@pbs!backup' "$CI_TOKEN" snapshot list --ns "$PBS_NS" --output-format json >"$WORK/snapshots.json"
count=$(grep -o '"backup-id":"app.data"' "$WORK/snapshots.json" | wc -l)
[[ $count == 2 ]] || fail "expected 2 snapshots of host/app.data, found $count: $(cat "$WORK/snapshots.json")"
grep -q '"backup-id":"app.skipped"' "$WORK/snapshots.json" && fail "skipped PVC was backed up"

log "no leftovers"
sel=app.kubernetes.io/managed-by=pbs-volume-backup
left=$(kubectl get pods,configmaps,pvc,volumesnapshots -A -l "$sel" -o name; kubectl get volumesnapshotcontents -l "$sel" -o name)
[[ -z $left ]] || fail "leftovers: $left"
for _ in $(seq 30); do
	retained=$(kubectl get pv -o jsonpath="{range .items[?(@.spec.claimRef.namespace=='$TOOL_NS')]}{.metadata.name} {end}")
	[[ -z $retained ]] && break
	sleep 2
done
[[ -z $retained ]] || fail "clone PVs left: $retained"

log "restore into a new PVC"
"$WORK/pvb" restore -v --from app/data --to app/restored --namespace "$TOOL_NS" \
	--image "$IMAGE" --repository "ci@pbs!backup@$PBS_IP:e2e" --fingerprint "$FINGERPRINT" \
	--pbs-namespace "$PBS_NS" --token-secret pbs-token
kubectl apply -n app -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: checker
spec:
  restartPolicy: Never
  containers:
    - name: checker
      image: busybox:1.37
      command:
        - sh
        - -c
        - |
          $(printf '%s' "$MANIFEST_SCRIPT" | sed '2,$s/^/          /')
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: restored
EOF
manifest app checker >"$WORK/restored.txt"
diff -u "$WORK/original.txt" "$WORK/restored.txt" || fail "restored files differ"
[[ -z $(kubectl get secrets -A -l "$sel" -o name) ]] || fail "token copy left behind"

log "PASS"
