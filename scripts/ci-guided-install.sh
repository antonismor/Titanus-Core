#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
fixture=$(mktemp -d /tmp/titanus-guided.XXXXXX)
trap 'rm -rf -- "$fixture"' EXIT
make build
TITANUS_RELEASE_OUTPUT="$fixture/packages" bash scripts/package-release.sh
native=$(go env GOARCH)
foreign=arm64
if [[ "$native" == arm64 ]]; then foreign=amd64; fi
version=$(./bin/titanus version --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')
revision=$(git rev-parse HEAD)
mkdir -p "$fixture/foreign/bin"
for binary in titanus titanusd titanus-agent titanus-init; do
 GOOS=linux GOARCH="$foreign" CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w -X github.com/antonismor/Titanus-Core/internal/version.Version=$version -X github.com/antonismor/Titanus-Core/internal/version.Revision=$revision" -o "$fixture/foreign/bin/$binary" "./cmd/$binary"
done
# Foreign ELF inventory is a private fixture, never executed, published or
# represented as native test evidence. The native package uses the publisher's
# exact regular package path and verifies all four binary identities.
python3 - "$fixture" "$version" "$revision" "$foreign" <<'PY'
import hashlib,json,pathlib,shutil,sys,tarfile
fixture=pathlib.Path(sys.argv[1]);version,revision,arch=sys.argv[2:]
root=fixture/f'titanus-{version}-linux-{arch}'
for name in ['bin/titanus','bin/titanusd','bin/titanus-agent','bin/titanus-init','systemd/titanusd.service','systemd/titanus-agent.service','scripts/install-release.py','scripts/install-release.sh','docs/RELEASE.md','LICENSE']:
 src=fixture/'foreign'/name if name.startswith('bin/') else pathlib.Path(name)
 dst=root/name;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(src,dst)
profile=json.loads(__import__('subprocess').check_output(['./bin/titanus','version','--json']))['state_profile']
(root/'manifest.json').write_text(json.dumps(dict(version=version,revision=revision,os='linux',arch=arch,state_profile=profile)))
(root/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.relative_to(root).as_posix()+'\n' for p in sorted(root.rglob('*')) if p.is_file()))
with tarfile.open(fixture/'packages'/(root.name+'.tar.gz'),'w:gz') as tar:tar.add(root,arcname=root.name)
PY
TITANUS_GUIDED_INSTALL_TEST=1 TITANUS_GUIDED_ARCHIVES="$fixture/packages" go test ./internal/deploy -run TestNativeGuidedInstallation -count=1 -v -timeout 120s
