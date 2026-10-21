#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d /tmp/titanus-trusted-browser.XXXXXX)
trap 'rm -rf "$work"' EXIT
export HOME="$work/home"
export TITANUS_INSTALLER_STATE_DIR="$work/state"
mkdir -m 700 -p "$HOME/.pki/nssdb"
"${TITANUS_INSTALLER_BINARY:-$PWD/bin/titanus-installer-web}" --state-dir "$TITANUS_INSTALLER_STATE_DIR" --prepare-tls
certutil -d "sql:$HOME/.pki/nssdb" -N --empty-password
certutil -d "sql:$HOME/.pki/nssdb" -A -n TitanusInstallerTestCA -t 'C,,' -i "$TITANUS_INSTALLER_STATE_DIR/https-ca.crt"
# Browser NSS and the Node API client both use real issuer trust. No global
# machine trust or TLS-error bypass is used; this HOME is deleted after testing.
export NODE_EXTRA_CA_CERTS="$TITANUS_INSTALLER_STATE_DIR/https-ca.crt"
node scripts/ci-web-installer.cjs
