#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Only a disposable container is changed. No SSH targets, volumes or devices
# from an operator's infrastructure are mounted or contacted.
docker run --rm -v "$PWD/bin/titanus:/usr/local/bin/titanus:ro" -v "$PWD/scripts/install-release.sh:/tmp/install-release.sh:ro" debian:13 bash -euo pipefail -c '
  ! command -v sshpass
  ! command -v python3
  bash /tmp/install-release.sh --prepare-host
  command -v python3
  command -v systemctl
  titanus installer prepare --host-only | tee /tmp/first.log
  for tool in ssh ssh-keyscan ssh-keygen sshpass python3 flock ip systemctl; do command -v "$tool"; done
  test -s /etc/ssl/certs/ca-certificates.crt
  titanus installer prepare --host-only | tee /tmp/second.log
  grep -q "All required packages are already installed" /tmp/second.log
  test ! -e /etc/titanus/daemon.env
  test ! -e /etc/titanus/agent.env
  test ! -e /var/lib/titanus/realm
  test ! -e /etc/ceph
  echo TITANUS_CLEAN_INSTALLER_HOST_OK
'
