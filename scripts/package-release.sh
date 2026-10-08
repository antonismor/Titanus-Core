#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
out="${TITANUS_RELEASE_OUTPUT:-dist}"
if [[ -n "$(git status --porcelain)" ]]; then echo "Release packaging requires a clean committed checkout." >&2; exit 1; fi
mkdir -p "$out"
python3 - "$out" <<'PY'
import hashlib,json,os,pathlib,re,shutil,subprocess,sys,tarfile,tempfile
out=pathlib.Path(sys.argv[1]).resolve()
meta=json.loads(subprocess.check_output(['./bin/titanus','version','--json']))
if meta['os']!='linux' or meta['arch'] not in ('amd64','arm64') or not re.fullmatch('[0-9a-f]{40}',meta['revision']) or meta['revision']!=subprocess.check_output(['git','rev-parse','HEAD']).decode().strip():raise SystemExit('release requires native Linux build at the exact committed revision')
if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?',meta['version']) or meta['state_profile']!='titanus-state/v2':raise SystemExit('invalid release version/state profile')
for binary in ['titanusd','titanus-agent','titanus-init']:
 if json.loads(subprocess.check_output(['./bin/'+binary,'--version-json']))!=meta:raise SystemExit('binary release identities do not match')
name=f"titanus-{meta['version']}-linux-{meta['arch']}"
with tempfile.TemporaryDirectory(prefix='titanus-package-') as tmp:
 root=pathlib.Path(tmp)/name;root.mkdir()
 for p in ['bin/titanus','bin/titanusd','bin/titanus-agent','bin/titanus-init','systemd/titanusd.service','systemd/titanus-agent.service','scripts/install-release.py','scripts/install-release.sh','docs/RELEASE.md','LICENSE']:
  src=pathlib.Path(p);dst=root/p;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(src,dst);dst.chmod(0o755 if p.startswith('bin/') or p.endswith('.sh') else 0o644)
 (root/'manifest.json').write_text(json.dumps(meta,sort_keys=True)+'\n')
 files=sorted(p for p in root.rglob('*') if p.is_file())
 (root/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.relative_to(root).as_posix()+'\n' for p in files))
 epoch=int(subprocess.check_output(['git','show','-s','--format=%ct',meta['revision']]))
 archive=out/(name+'.tar.gz')
 # Stable metadata and gzip timestamp make equivalent builds reproducible.
 import gzip
 with archive.open('wb') as stream,gzip.GzipFile(filename='',mode='wb',fileobj=stream,mtime=epoch) as gz,tarfile.open(fileobj=gz,mode='w') as tar:
  for p in [root,*sorted(root.rglob('*'))]:
   info=tar.gettarinfo(str(p),arcname=p.relative_to(root.parent).as_posix());info.uid=info.gid=0;info.uname=info.gname='root';info.mtime=epoch
   if p.is_file():
    with p.open('rb') as f:tar.addfile(info,f)
   else:tar.addfile(info)
 (out/(archive.name+'.sha256')).write_text(hashlib.sha256(archive.read_bytes()).hexdigest()+'  '+archive.name+'\n')
 print(archive)
PY
