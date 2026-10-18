#!/usr/bin/env bash
# Disposable native Ceph HA fixture. Sparse files only, inside a GitHub runner.
# No external hosts, block devices, infrastructure APIs or containers.
set -euo pipefail
root=$(mktemp -d /tmp/titanus-ceph-ha.XXXXXX)
conf="$root/ceph.conf"
export CEPH_ARGS="--conf=$conf"
if [[ $(uname -m) == aarch64 ]]; then export TCMALLOC_STACKTRACE_METHOD=generic_fp; fi
ceph() { timeout --kill-after=3s 20s /usr/bin/ceph "$@"; }
rados() { timeout --kill-after=3s 45s /usr/bin/rados "$@"; }
pids=()
cleanup() {
 status=$?
 if (( status != 0 )); then ceph -s || true; tail -n 60 "$root"/*.stdout || true; fi
 for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
 for attempt in $(seq 1 10); do running=0; for pid in "${pids[@]}"; do if kill -0 "$pid" 2>/dev/null; then running=1; fi; done; if (( running == 0 )); then break; fi; sleep 1; done
 for pid in "${pids[@]}"; do kill -KILL "$pid" 2>/dev/null || true; done
 wait || true
}
trap cleanup EXIT
uuid=$(cat /proc/sys/kernel/random/uuid)
cat > "$conf" <<CFG
[global]
fsid = $uuid
mon host = 127.0.0.51,127.0.0.52,127.0.0.53
public network = 127.0.0.0/8
cluster network = 127.0.0.0/8
auth cluster required = cephx
auth service required = cephx
auth client required = cephx
keyring = $root/admin.keyring
osd pool default size = 3
osd pool default min size = 2
osd crush chooseleaf type = 1
osd data = $root/osd.\$id
mon data = $root/mon.\$id
mgr data = $root/mgr.\$id
run dir = $root
log file = $root/\$name.log
pid file = $root/\$name.pid
mon data avail warn = 2
mon data avail crit = 1
[mon]
keyring = $root/mon.keyring
[mon.a]
mon addr = 127.0.0.51:6789
[mon.b]
mon addr = 127.0.0.52:6789
[mon.c]
mon addr = 127.0.0.53:6789
[osd]
keyring = $root/osd.\$id/keyring
osd memory target = 536870912
osd heartbeat grace = 10
osd objectstore = bluestore
[mgr]
keyring = $root/mgr.\$id/keyring
CFG
ceph-authtool --create-keyring "$root/mon.keyring" --gen-key -n mon. --cap mon 'allow *'
ceph-authtool --create-keyring "$root/admin.keyring" --gen-key -n client.admin --cap mon 'allow *' --cap mgr 'allow *' --cap osd 'allow *'
ceph-authtool "$root/mon.keyring" --import-keyring "$root/admin.keyring"
monmaptool --create --fsid "$uuid" --add a 127.0.0.51 --add b 127.0.0.52 --add c 127.0.0.53 "$root/monmap"
for name in a b c; do
 mkdir -p "$root/mon.$name"
 ceph-mon -i "$name" --mkfs --monmap "$root/monmap" --keyring "$root/mon.keyring" -c "$conf"
 ceph-mon -i "$name" -f -c "$conf" >"$root/mon.$name.stdout" 2>&1 & pid=$!; pids+=("$pid"); if [[ $name == a ]]; then mon_a_pid=$pid; fi
 done
ready=0
for attempt in $(seq 1 60); do if ceph quorum_status -f json | jq -e '(.quorum_names|sort)==["a","b","c"]' >/dev/null; then ready=1; break; fi; sleep 1; done
if (( ready == 0 )); then echo 'Three monitor quorum unavailable' >&2; exit 1; fi
ceph mon enable-msgr2
ceph config set mon auth_allow_insecure_global_id_reclaim false
ceph auth get-or-create mgr.a mon 'profile mgr' osd 'allow *' -o "$root/mgr.a.keyring"
mkdir -p "$root/mgr.a"; cp "$root/mgr.a.keyring" "$root/mgr.a/keyring"
ceph-mgr -i a -f -c "$conf" >"$root/mgr.stdout" 2>&1 & pids+=("$!")
for id in 0 1 2; do
 osd_uuid=$(cat /proc/sys/kernel/random/uuid)
 actual=$(ceph osd new "$osd_uuid"); test "$actual" = "$id"
 mkdir -p "$root/osd.$id"
 ceph auth get-or-create "osd.$id" mon 'profile osd' mgr 'profile osd' osd 'allow *' -o "$root/osd.$id/keyring"
 truncate -s 4G "$root/osd.$id/block"
 ceph-osd -i "$id" --mkfs --osd-uuid "$osd_uuid" -c "$conf"
 host=$(printf '\\%03o' "$((97 + id))"); host=$(printf '%b' "$host")
 ceph osd crush add "osd.$id" 1 root=default host="$host"
 ceph-osd -i "$id" -f -c "$conf" --no-mon-config --monmap "$root/monmap" >"$root/osd.$id.stdout" 2>&1 & pid=$!; pids+=("$pid"); if (( id == 0 )); then osd_0_pid=$pid; fi
 done
ceph osd crush rule create-replicated titanus-ha-host default host
ceph osd pool create titanus 8 8 replicated titanus-ha-host
ceph osd pool set titanus size 3
ceph osd pool set titanus min_size 2
ceph osd pool application enable titanus rbd
wait_clean() {
 for attempt in $(seq 1 120); do
  if ceph status -f json | jq -e '.health.status=="HEALTH_OK" and .pgmap.num_pgs>0 and all(.pgmap.pgs_by_state[]; .state_name=="active+clean")' >/dev/null; then return; fi
  sleep 1
 done
 echo 'Ceph HA fixture failed to become clean' >&2; return 1
}
wait_clean
export TITANUS_CEPH_HA_TEST=1
# Execute the production report collector and validator against actual daemons.
go test ./internal/deploy -run '^TestNativeThreeMonitorHAPolicy$' -count=1 -v -timeout 120s
printf 'BEFORE_FAILURE_%s\n' "$uuid" > "$root/before"
rados -p titanus put ha-proof "$root/before"
# Lose one planned monitor and one OSD failure domain together. Remaining
# quorum and min_size=2 must permit real writes; the HA readiness gate refuses
# activation until the entire planned storage topology is healthy again.
kill -KILL "$mon_a_pid" "$osd_0_pid"
wait "$mon_a_pid" || true; wait "$osd_0_pid" || true
ready=0
for attempt in $(seq 1 90); do
 if ceph quorum_status -f json | jq -e '(.quorum_names|length)==2' >/dev/null && ceph osd dump -f json | jq -e '[.osds[]|select(.up==1)]|length==2' >/dev/null; then ready=1; break; fi
 sleep 1
 done
if (( ready == 0 )); then echo 'Native storage failure was not observed' >&2; exit 1; fi
TITANUS_CEPH_HA_DEGRADED=1 go test ./internal/deploy -run '^TestNativeThreeMonitorHAPolicy$' -count=1 -v -timeout 120s
rados -p titanus get ha-proof "$root/read-before"; cmp "$root/before" "$root/read-before"
printf 'DURING_FAILURE_%s\n' "$uuid" > "$root/during"
rados -p titanus put ha-proof "$root/during"
ceph-mon -i a -f -c "$conf" >"$root/mon.a.rejoin.stdout" 2>&1 & pids+=("$!")
ceph-osd -i 0 -f -c "$conf" --no-mon-config --monmap "$root/monmap" >"$root/osd.0.rejoin.stdout" 2>&1 & pids+=("$!")
wait_clean
go test ./internal/deploy -run '^TestNativeThreeMonitorHAPolicy$' -count=1 -v -timeout 120s
rados -p titanus get ha-proof "$root/read-after"; cmp "$root/during" "$root/read-after"
echo TITANUS_NATIVE_THREE_MONITOR_HOST_DOMAIN_STORAGE_RECOVERY_OK
