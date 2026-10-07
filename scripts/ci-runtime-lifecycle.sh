#!/usr/bin/env bash
set -euo pipefail
task_state=/tmp/titanus-ci-state
task_cgroup=/sys/fs/cgroup/titanus-ci
export TITANUS_STATE_ROOT="$task_state" TITANUS_CGROUP_ROOT="$task_cgroup"
export TITANUS_INIT_BINARY="$PWD/bin/titanus-init"
unit() { ./bin/titanus unit "$@"; }
daemon_pid=
cleanup() {
  if [[ -n "$daemon_pid" ]]; then kill "$daemon_pid" 2>/dev/null || true; wait "$daemon_pid" 2>/dev/null || true; fi
  unit stop lifecycle 2>/dev/null || true
}
trap cleanup EXIT

unit create lifecycle --source busybox --memory 64M --pids 64 -- /bin/sh -ec '
  test "$(/bin/busybox cat /proc/1/cmdline | /bin/busybox tr "\000" " ")" != ""
  /bin/busybox grep -aq -- --supervise /proc/1/cmdline
  (sleep 0.1 &)
  sleep 0.5
  for path in /proc/[0-9]*/stat; do
    read -r pid comm state rest < "$path" || continue
    test "$state" != Z
  done
  trap "exit 23" TERM
  echo REAPING_OK
  while :; do sleep 1; done
'
unit start lifecycle
for attempt in {1..50}; do
  unit logs lifecycle | grep -q REAPING_OK && break
  sleep 0.1
done
unit logs lifecycle | grep -q REAPING_OK
before=$(unit inspect lifecycle | jq -c '.state | {pid,process,run_id}')

start_daemon() {
  ./bin/titanusd > /tmp/titanus-lifecycle-daemon.log 2>&1 & daemon_pid=$!
  for attempt in {1..50}; do
    if curl --fail --silent --unix-socket /run/titanus/titanus.sock http://localhost/v1/health >/dev/null; then return; fi
    sleep 0.1
  done
  cat /tmp/titanus-lifecycle-daemon.log
  return 1
}
start_daemon
kill -KILL "$daemon_pid"
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=
start_daemon
after=$(unit inspect lifecycle | jq -c '.state | {pid,process,run_id}')
test "$before" = "$after"
unit inspect lifecycle | jq -e '.state.status == "ACTIVE"'
unit stop lifecycle
unit inspect lifecycle | jq -e '.state.status == "STOPPED" and .state.exit_code == 23'
test ! -d "$task_cgroup/lifecycle"
unit delete lifecycle

unit create exit-seven --source busybox -- /bin/sh -c 'sleep 0.2; exit 7'
unit start exit-seven
for attempt in {1..50}; do
  unit inspect exit-seven | jq -e '.state.exit_code == 7' >/dev/null && break
  sleep 0.1
done
unit inspect exit-seven | jq -e '.state.status == "FAILED" and .state.exit_code == 7'
test ! -d "$task_cgroup/exit-seven"
unit delete exit-seven
echo TITANUS_LIFECYCLE_OK
