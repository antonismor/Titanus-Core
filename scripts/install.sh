#!/usr/bin/env bash
set -euo pipefail
if [[ "${EUID}" -ne 0 ]]; then echo 'Titanus installer must run as root.' >&2; exit 1; fi
cd "$(dirname "$0")/.."
make release
archive=$(python3 - <<'PY'
import json,subprocess
m=json.loads(subprocess.check_output(['./bin/titanus','version','--json']))
print(f"dist/titanus-{m['version']}-linux-{m['arch']}.tar.gz")
PY
)
(cd dist && sha256sum -c "$(basename "$archive").sha256")
stage=$(mktemp -d /tmp/titanus-source-install.XXXXXX)
tar -xzf "$archive" -C "$stage"
bundle=$(find "$stage" -mindepth 1 -maxdepth 1 -type d -print -quit)
python3 scripts/install-release.py --bundle "$bundle"
echo 'Configure Realm/PKI/daemon.env/agent.env, then start the configured services.'
