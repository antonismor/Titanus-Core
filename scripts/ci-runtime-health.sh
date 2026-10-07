#!/usr/bin/env bash
set -euo pipefail
export TITANUS_STATE_ROOT=/tmp/titanus-ci-state
export TITANUS_CGROUP_ROOT=/sys/fs/cgroup/titanus-ci
export TITANUS_INIT_BINARY="$PWD/bin/titanus-init"
unit() { ./bin/titanus unit "$@"; }
daemon_pid=
cleanup() {
  unit stop health-smoke 2>/dev/null || true
  if [[ -n "$daemon_pid" ]]; then kill "$daemon_pid" 2>/dev/null || true; wait "$daemon_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT
cat > /tmp/titanus-health.json <<'JSON'
{"readiness":{"protocol":"http","port":8080,"path":"/ready","interval_seconds":1,"failure_threshold":1},"liveness":{"protocol":"http","port":8080,"path":"/live","interval_seconds":1,"failure_threshold":1},"restart":"on-failure","initial_backoff_seconds":2,"max_backoff_seconds":8,"max_restarts":3}
JSON
./bin/titanus disk create health-data --provider local --size 64M
# Test fixture only; production data must have the mapped writer ownership.
chmod 0777 "$TITANUS_STATE_ROOT/disks/health-data/data"
unit create health-smoke --source busybox --memory 64M --pids 64 --mount health-data:/data \
  --health-config /tmp/titanus-health.json -- /bin/health-app
unit start health-smoke
unit inspect health-smoke | jq -e '.state.status == "ACTIVE" and .state.ready == false'
./bin/titanusd > /tmp/titanus-health-daemon.log 2>&1 & daemon_pid=$!
for attempt in {1..150}; do
  unit inspect health-smoke | jq -e '.state.ready and .state.restart_count == 0' >/dev/null && break
  sleep 0.1
done
unit inspect health-smoke | jq -e '.state.ready and .state.restart_count == 0'
before=$(unit inspect health-smoke | jq -r '.state.run_id')
for attempt in {1..200}; do
  unit inspect health-smoke | jq -e '.state.restart_count == 1 and .state.ready' >/dev/null && break
  sleep 0.1
done
unit inspect health-smoke | jq -e '.state.status == "ACTIVE" and .state.restart_count == 1 and .state.ready'
after=$(unit inspect health-smoke | jq -r '.state.run_id')
test "$before" != "$after"
test "$(cat "$TITANUS_STATE_ROOT/disks/health-data/data/starts")" = 2
kill -KILL "$daemon_pid"; wait "$daemon_pid" 2>/dev/null || true
./bin/titanusd >> /tmp/titanus-health-daemon.log 2>&1 & daemon_pid=$!
sleep 1
unit inspect health-smoke | jq -e '.state.status == "ACTIVE" and .state.restart_count == 1 and .state.ready'
unit stop health-smoke
sleep 3
unit inspect health-smoke | jq -e '.state.status == "STOPPED" and .state.desired_running == false and .state.ready == false'
test "$(cat "$TITANUS_STATE_ROOT/disks/health-data/data/starts")" = 2
unit delete health-smoke
echo TITANUS_HEALTH_OK
