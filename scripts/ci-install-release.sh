#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
fixture=$(mktemp -d /tmp/titanus-install-fixture.XXXXXX)
cleanup() {
 if [[ "${TITANUS_INSTALL_LIVE_TEST:-0}" == 1 ]]; then
  /usr/local/bin/titanus unit stop install-proof >/dev/null 2>&1 || true
  systemctl stop titanusd.service || true
 fi
}
trap cleanup EXIT
# Build two independently identified compatible versions and restore the release
# build afterward. Fixture versions never enter the publication artifact folder.
make build
base_version=$(./bin/titanus version --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')
next_version="${base_version}.install-test"
TITANUS_RELEASE_OUTPUT="$fixture/base" bash scripts/package-release.sh
make build VERSION="$next_version"
TITANUS_PACKAGE_INSTALL_FIXTURE=1 TITANUS_RELEASE_OUTPUT="$fixture/next" bash scripts/package-release.sh
make build
for kind in base next; do
 archive=$(find "$fixture/$kind" -maxdepth 1 -name '*.tar.gz' -print -quit)
 (cd "$fixture/$kind" && sha256sum -c "$(basename "$archive").sha256")
 tar -xzf "$archive" -C "$fixture/$kind"
done
base=$(find "$fixture/base" -mindepth 1 -maxdepth 1 -type d -print -quit)
next=$(find "$fixture/next" -mindepth 1 -maxdepth 1 -type d -print -quit)
stage="$fixture/stage"
python3 scripts/install-release.py --root "$stage" --bundle "$base"
mkdir -p "$stage/etc/titanus" "$stage/var/lib/titanus"
printf '%s\n' 'CONFIG_PRESERVED' > "$stage/etc/titanus/daemon.env"
printf '%s\n' 'MAPPING_LEDGER_PRESERVED' > "$stage/var/lib/titanus/ledger-canary"
python3 scripts/install-release.py --root "$stage" --bundle "$next"
"$stage/usr/local/bin/titanus" version --json | python3 -c 'import json,sys; assert json.load(sys.stdin)["version"]==sys.argv[1]' "$next_version"
python3 scripts/install-release.py --root "$stage" --rollback
"$stage/usr/local/bin/titanus" version --json | python3 -c 'import json,sys; assert json.load(sys.stdin)["version"]==sys.argv[1]' "$base_version"
grep -q CONFIG_PRESERVED "$stage/etc/titanus/daemon.env"
grep -q MAPPING_LEDGER_PRESERVED "$stage/var/lib/titanus/ledger-canary"
selection=$(readlink "$stage/usr/local/lib/titanus/current")
python3 - "$next/bin/titanus" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]);data=bytearray(p.read_bytes());data[-1]^=1;p.write_bytes(data)
PY
if python3 scripts/install-release.py --root "$stage" --bundle "$next"; then
 echo 'Corrupt bundle accepted' >&2; exit 1
fi
test "$(readlink "$stage/usr/local/lib/titanus/current")" = "$selection"
# Restore the fixture's corrupted byte from its verified archive.
tar -xzf "$(find "$fixture/next" -maxdepth 1 -name '*.tar.gz' -print -quit)" -C "$fixture/next"
if [[ "${TITANUS_INSTALL_LIVE_TEST:-0}" == 1 ]]; then
 python3 scripts/install-release.py --bundle "$base"
 mkdir -p /etc/titanus
 cat > /etc/titanus/daemon.env <<ENV
TITANUS_STATE_ROOT=$fixture/live-state
TITANUS_SOCKET=$fixture/live.sock
TITANUS_INIT_BINARY=/usr/local/libexec/titanus-init
TITANUS_CGROUP_ROOT=/sys/fs/cgroup/titanus-install-ci
ENV
 wait_health() {
  for attempt in $(seq 1 50); do
   if curl -fsS --unix-socket "$fixture/live.sock" http://titanus.local/v1/health >/dev/null 2>&1; then return; fi
   sleep .2
  done
  systemctl status titanusd.service --no-pager || true
  return 1
 }
 systemctl daemon-reload
 systemctl start titanusd.service
 wait_health
 export TITANUS_STATE_ROOT="$fixture/live-state" TITANUS_CGROUP_ROOT=/sys/fs/cgroup/titanus-install-ci TITANUS_INIT_BINARY=/usr/local/libexec/titanus-init TITANUS_SOCKET="$fixture/live.sock"
 /usr/local/bin/titanus source import app /tmp/titanus-rootfs
 /usr/local/bin/titanus unit create install-proof --source app --memory 64M --cpu 50 --pids 64 -- /bin/sleep 180
 /usr/local/bin/titanus unit start install-proof
 pid=$(/usr/local/bin/titanus unit inspect install-proof | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"]["pid"])')
 python3 scripts/install-release.py --bundle "$next"
 wait_health
 /usr/local/bin/titanus unit inspect install-proof | python3 -c 'import json,sys; s=json.load(sys.stdin)["state"]; assert s["status"]=="ACTIVE" and s["pid"]==int(sys.argv[1])' "$pid"
 curl -fsS --unix-socket "$fixture/live.sock" http://titanus.local/v1/version | python3 -c 'import json,sys; assert json.load(sys.stdin)["version"]==sys.argv[1]' "$next_version"
 python3 scripts/install-release.py --rollback
 wait_health
 /usr/local/bin/titanus unit inspect install-proof | python3 -c 'import json,sys; s=json.load(sys.stdin)["state"]; assert s["status"]=="ACTIVE" and s["pid"]==int(sys.argv[1])' "$pid"
 curl -fsS --unix-socket "$fixture/live.sock" http://titanus.local/v1/version | python3 -c 'import json,sys; assert json.load(sys.stdin)["version"]==sys.argv[1]' "$base_version"
fi
printf '%s\n' TITANUS_NATIVE_INSTALL_UPGRADE_ROLLBACK_OK
