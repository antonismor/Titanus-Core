#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "Titanus installer must run as root." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build Titanus Core." >&2
  exit 1
fi

echo "==> Building Titanus Core"
cd "${ROOT_DIR}"
make build

echo "==> Installing binaries"
install -d -m 0755 /usr/local/libexec
install -m 0755 bin/titanus /usr/local/bin/titanus
install -m 0755 bin/titanusd /usr/local/sbin/titanusd
install -m 0755 bin/titanus-init /usr/local/libexec/titanus-init

echo "==> Creating Titanus filesystem"
install -d -m 0755 /etc/titanus
install -d -m 0755 /var/lib/titanus
install -d -m 0755 /var/lib/titanus/runtime
install -d -m 0755 /var/lib/titanus/sources
install -d -m 0755 /var/lib/titanus/disks
install -d -m 0755 /var/log/titanus
install -d -m 0755 /run/titanus

echo "==> Installing systemd service"
install -m 0644 systemd/titanusd.service /etc/systemd/system/titanusd.service
systemctl daemon-reload
systemctl enable titanusd.service

echo
echo "Titanus Core installed."
echo "Start the guided setup with: titanus"
echo "Start the daemon with:       systemctl start titanusd"
