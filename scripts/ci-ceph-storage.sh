#!/usr/bin/env bash
# Ephemeral native Ceph daemons on a dedicated GitHub runner. No containers.
set -euo pipefail
root=$(mktemp -d /tmp/titanus-ceph.XXXXXX)
conf="$root/ceph.conf"
export CEPH_ARGS="--conf=$conf"
pids=()
ulimit -c unlimited
echo "$root/core.%e.%p" > /proc/sys/kernel/core_pattern
cleanup() {
  status=$?
  if (( status != 0 )); then
    ceph -s || true
    tail -n 80 "$root"/*.stdout "$root"/*.log || true
  fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait || true
}
trap cleanup EXIT
uuid=$(cat /proc/sys/kernel/random/uuid)
cat > "$conf" <<CFG
[global]
fsid = $uuid
mon host = 127.0.0.1
public network = 127.0.0.0/8
cluster network = 127.0.0.0/8
auth cluster required = none
auth service required = none
auth client required = none
mon allow pool delete = true
mon allow pool size one = true
osd pool default size = 1
osd pool default min size = 1
osd crush chooseleaf type = 0
osd objectstore = bluestore
osd data = $root/osd.\$id
mon data = $root/mon.\$id
mgr data = $root/mgr.\$id
mds data = $root/mds.\$id
run dir = $root
log file = $root/\$name.log
pid file = $root/\$name.pid
[osd]
osd memory target = 536870912
[mds]
mds session blocklist on evict = true
mds session blocklist on timeout = true
CFG
mkdir -p "$root"/{mon.a,mgr.a,mds.a,osd.0}
monmaptool --create --add a 127.0.0.1 --fsid "$uuid" "$root/monmap"
ceph-mon -i a --mkfs --monmap "$root/monmap" -c "$conf"
ceph-mon -i a -f -c "$conf" >"$root/mon.stdout" 2>&1 & pids+=("$!")
for attempt in $(seq 1 60); do if timeout 3 ceph -s >/dev/null 2>&1; then break; fi; sleep 1; done
ceph -s
osd_uuid=$(cat /proc/sys/kernel/random/uuid)
ceph osd new "$osd_uuid"
truncate -s 4G "$root/osd.0/block"
if ! ceph-osd -i 0 --mkfs --osd-uuid "$osd_uuid" -c "$conf"; then
  lscpu
  for core in "$root"/core.ceph-osd.*; do
    gdb -batch -ex 'thread apply all bt' -ex 'x/8i $pc' /usr/bin/ceph-osd "$core" || true
  done
  exit 1
fi
ceph osd crush add osd.0 1 root=default host=ci
ceph-osd -i 0 -f -c "$conf" >"$root/osd.stdout" 2>&1 & pids+=("$!")
ceph-mgr -i a -f -c "$conf" >"$root/mgr.stdout" 2>&1 & pids+=("$!")
for attempt in $(seq 1 60); do if ceph mgr dump -f json | jq -e '.active_name == "a"' >/dev/null; then break; fi; sleep 1; done
ceph osd pool create titanus 8
rbd pool init titanus
ceph osd pool create cephfs_meta 8
ceph osd pool create cephfs_data 8
ceph fs new cephfs cephfs_meta cephfs_data --force
ceph-mds -i a -f -c "$conf" >"$root/mds.stdout" 2>&1 & pids+=("$!")
ceph mgr module enable volumes
for attempt in $(seq 1 90); do if ceph fs status cephfs -f json | jq -e '.mdsmap[] | select(.state == "active")' >/dev/null; then break; fi; sleep 1; done
ceph fs status
modprobe rbd
modprobe ceph
export TITANUS_CEPH_TEST=1 TITANUS_CEPH_CONF="$conf"
if ! go test ./internal/disk -run '^TestNativeCeph' -count=1 -v -timeout 12m; then
  ceph -s || true
  tail -n 80 "$root"/*.stdout "$root"/*.log || true
  exit 1
fi
