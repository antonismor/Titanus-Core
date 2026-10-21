#!/usr/bin/env bash
set -euo pipefail
# The native archive's entry point must also work on a minimal Debian host
# without Python. Offline --root activation never installs host packages.
offline_root=false
for ((i=1; i<=$#; i++)); do
  if [[ ${!i} == --root ]]; then
    j=$((i+1)); [[ ${!j:-/} == / ]] || offline_root=true
  elif [[ ${!i} == --root=* && ${!i} != --root=/ ]]; then
    offline_root=true
  fi
done
if ! command -v python3 >/dev/null 2>&1 || { [[ $offline_root == false ]] && ! command -v systemctl >/dev/null 2>&1; }; then
  if [[ $offline_root == true ]]; then echo 'Python 3 is required for offline --root activation; no host packages were changed.' >&2; exit 1; fi
  if [[ $EUID -ne 0 ]] || ! command -v apt-get >/dev/null 2>&1 || [[ ! -r /etc/debian_version ]]; then
    echo 'Initial release activation requires root on a supported Debian-family host.' >&2; exit 1
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq -o APT::Update::Error-Mode=any -o DPkg::Lock::Timeout=120 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30
  apt-get install -y --no-install-recommends --no-upgrade -o DPkg::Lock::Timeout=120 python3 systemd ca-certificates
fi
if [[ ${1:-} == --prepare-host && $# == 1 ]]; then exit 0; fi
exec python3 "$(dirname "$0")/install-release.py" "$@"
