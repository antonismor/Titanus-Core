#!/usr/bin/env python3
"""Verified versioned activation; shared state/configuration are never rewritten."""
import argparse,fcntl,hashlib,json,os,pathlib,platform,re,shutil,stat,subprocess,sys,tempfile
p=argparse.ArgumentParser();p.add_argument('--bundle',type=pathlib.Path);p.add_argument('--root',type=pathlib.Path,default=pathlib.Path('/'));p.add_argument('--rollback',action='store_true');p.add_argument('--config',type=pathlib.Path);p.add_argument('--expect-version');p.add_argument('--expect-revision');args=p.parse_args()
if bool(args.bundle)==args.rollback:p.error('choose --bundle DIRECTORY or --rollback')
if not args.root.is_absolute() or args.root.is_symlink():p.error('installation root must be an absolute real directory')
root=args.root.resolve();live=root==pathlib.Path('/')
if live and os.geteuid()!=0:p.error('live installation requires root')
if live:
 maintenance_fd=os.open('/run/titanus-offline.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW|os.O_CLOEXEC,0o600)
 maintenance_stat=os.fstat(maintenance_fd)
 if not stat.S_ISREG(maintenance_stat.st_mode) or maintenance_stat.st_uid!=0 or maintenance_stat.st_mode&0o777!=0o600:raise SystemExit('unsafe offline maintenance lock')
 try:fcntl.flock(maintenance_fd,fcntl.LOCK_SH|fcntl.LOCK_NB)
 except BlockingIOError:raise SystemExit('offline Titanus maintenance is active')
 for suffix in ['.recovery-pending','.backup-pending']:
  if os.path.lexists('/var/lib/titanus'+suffix):raise SystemExit('unfinished offline maintenance blocks installation')
base=root/'usr/local/lib/titanus';base.mkdir(parents=True,exist_ok=True)
def syncdir(path):
 fd=os.open(path,os.O_RDONLY|os.O_DIRECTORY)
 try:os.fsync(fd)
 finally:os.close(fd)
def atomiclink(target,path):
 temp=path.with_name(path.name+'.next')
 if temp.exists() or temp.is_symlink():temp.unlink()
 temp.symlink_to(target);os.replace(temp,path);syncdir(path.parent)
def verified(bundle,inspect_binaries=True,checksum_data=None):
 allowed={'bin/titanus','bin/titanusd','bin/titanus-agent','bin/titanus-init','systemd/titanusd.service','systemd/titanus-agent.service','scripts/install-release.py','scripts/install-release.sh','docs/RELEASE.md','LICENSE','manifest.json'}
 checksum=bundle/'SHA256SUMS'
 if checksum.is_symlink() or not checksum.is_file():raise ValueError('missing regular checksum list')
 seen=set()
 for line in (checksum_data.decode() if checksum_data is not None else checksum.read_text()).splitlines():
  digest,name=line.split('  ',1)
  if name not in allowed or name in seen or not re.fullmatch('[0-9a-f]{64}',digest):raise ValueError('invalid release checksum entry')
  src=bundle/name
  if src.is_symlink() or not src.is_file() or hashlib.sha256(src.read_bytes()).hexdigest()!=digest:raise ValueError('release checksum mismatch: '+name)
  seen.add(name)
 if seen!=allowed:raise ValueError('incomplete release')
 meta=json.loads((bundle/'manifest.json').read_text())
 if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?',meta['version']) or not re.fullmatch('[0-9a-f]{40}',meta['revision']):raise ValueError('invalid release identity')
 arch={'x86_64':'amd64','aarch64':'arm64'}.get(platform.machine())
 if meta['os']!='linux' or meta['arch']!=arch or meta['state_profile']!='titanus-state/v3':raise ValueError('unsupported release architecture/state profile')
 for name in (['titanus','titanusd','titanus-agent','titanus-init'] if inspect_binaries else []):
  command=[str(bundle/'bin'/name),'version','--json'] if name=='titanus' else [str(bundle/'bin'/name),'--version-json']
  if json.loads(subprocess.check_output(command,timeout=5))!=meta:raise ValueError('binary manifest identity mismatch')
 return meta
lock=(base/'install.lock').open('a');os.chmod(base/'install.lock',0o600);fcntl.flock(lock,fcntl.LOCK_EX)
current=base/'current';previous=base/'previous';old=os.readlink(current) if current.is_symlink() else None
services=[];created_links=[];created_config=[];switched=False
prior_previous=os.readlink(previous) if previous.is_symlink() else None
if current.exists() and not current.is_symlink():raise SystemExit('current selector must be a symlink')
try:
 if args.rollback:
  if not previous.is_symlink():raise ValueError('no prior versioned release')
  target=os.readlink(previous);candidate=(base/target).resolve()
  if candidate.parent!=(base/'releases').resolve():raise ValueError('invalid previous release path')
  meta=verified(candidate)
 else:
  bundle=args.bundle.resolve();proof=(bundle/'SHA256SUMS').read_bytes();meta=verified(bundle,False,proof)
  release_id=meta['version']+'-'+meta['revision'][:12];target='releases/'+release_id
  releases=base/'releases';releases.mkdir(exist_ok=True);candidate=base/target
  if candidate.exists():
   if verified(candidate)!=meta:raise ValueError('release directory identity collision')
  else:
   stage=pathlib.Path(tempfile.mkdtemp(prefix='.install-',dir=releases))
   try:
    for line in proof.decode().splitlines():
     _,name=line.split('  ',1);src=bundle/name;dst=stage/name;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(src,dst);dst.chmod(0o755 if name.startswith('bin/') or name.endswith('.sh') else 0o644)
     with dst.open('rb') as f:os.fsync(f.fileno())
    (stage/'SHA256SUMS').write_bytes(proof)
    with (stage/'SHA256SUMS').open('rb') as f:os.fsync(f.fileno())
    for folder in sorted((x for x in stage.rglob('*') if x.is_dir()),reverse=True):syncdir(folder)
    syncdir(stage);verified(stage);os.rename(stage,candidate);syncdir(releases)
   finally:
    if stage.exists():shutil.rmtree(stage)
 if args.expect_version and meta['version']!=args.expect_version:raise ValueError('selected version differs from requested target')
 if args.expect_revision and meta['revision']!=args.expect_revision:raise ValueError('selected revision differs from requested tested SHA')
 configuration={}
 if args.config:
  if args.rollback or old:raise ValueError('configuration is for initial installation only')
  config=args.config.resolve();inventory=json.loads((config/'config.json').read_text())
  allowed_config={'daemon.env','agent.env','ha.json','pki/ca.crt','pki/ca.crl','pki/ca.key','pki/node.crt','pki/node.key'}
  required={'daemon.env','agent.env','pki/ca.crt','pki/ca.crl','pki/node.crt','pki/node.key'}
  if not required.issubset(inventory) or not set(inventory).issubset(allowed_config):raise ValueError('invalid initial configuration inventory')
  for name,digest in inventory.items():
   src=config/name;dst=root/'etc/titanus'/name
   if src.is_symlink() or not src.is_file() or not re.fullmatch('[0-9a-f]{64}',digest):raise ValueError('invalid configuration file')
   data=src.read_bytes()
   if hashlib.sha256(data).hexdigest()!=digest:raise ValueError('configuration checksum mismatch')
   if dst.exists() or dst.is_symlink() or any(folder.is_symlink() for folder in dst.parents):raise ValueError('initial configuration refuses existing or redirected path')
   configuration[dst]=data
 links={'usr/local/bin/titanus':'../lib/titanus/current/bin/titanus','usr/local/sbin/titanusd':'../lib/titanus/current/bin/titanusd','usr/local/sbin/titanus-agent':'../lib/titanus/current/bin/titanus-agent','usr/local/libexec/titanus-init':'../lib/titanus/current/bin/titanus-init','etc/systemd/system/titanusd.service':'../../../usr/local/lib/titanus/current/systemd/titanusd.service','etc/systemd/system/titanus-agent.service':'../../../usr/local/lib/titanus/current/systemd/titanus-agent.service'}
 # Validate every destination before stopping services or changing any link.
 for name,link in links.items():
  dst=root/name
  if dst.exists() or dst.is_symlink():
   if not dst.is_symlink() or os.readlink(dst)!=link:raise ValueError('unversioned/custom installation needs explicit migration: '+name)
 if old:
  oldpath=(base/old).resolve()
  if oldpath.parent!=(base/'releases').resolve() or verified(oldpath)['state_profile']!=meta['state_profile']:raise ValueError('incompatible current state profile')
 if live:
  for service in ['titanus-agent.service','titanusd.service']:
   if subprocess.run(['systemctl','is-active','--quiet',service]).returncode==0:services.append(service)
  for service in services:subprocess.run(['systemctl','stop',service],check=True)
 for name,link in links.items():
  dst=root/name;dst.parent.mkdir(parents=True,exist_ok=True)
  if not dst.is_symlink():
   atomiclink(link,dst);created_links.append(dst)
 for name in ['etc/titanus','var/lib/titanus','var/log/titanus','run/titanus']:(root/name).mkdir(parents=True,exist_ok=True)
 for dst,data in configuration.items():
  dst.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
  if str(dst.parent).endswith('/pki'):dst.parent.chmod(0o700)
  fd=os.open(dst,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);created_config.append(dst)
  with os.fdopen(fd,'wb') as stream:stream.write(data);stream.flush();os.fsync(stream.fileno())
  syncdir(dst.parent)
 atomiclink(target,current);switched=True
 if old and old!=target:atomiclink(old,previous)
 if live:
  subprocess.run(['systemctl','daemon-reload'],check=True)
  for service in reversed(services):subprocess.run(['systemctl','start',service],check=True)
 print(json.dumps({'activated':meta,'previous':old,'live_services_restarted':services}))
except Exception as error:
 # Best-effort restore of binary selection; state is never claimed rolled back.
 if old and current.is_symlink() and os.readlink(current)!=old:
  atomiclink(old,current)
 if switched:
  if not old and current.is_symlink():current.unlink();syncdir(base)
  if prior_previous:atomiclink(prior_previous,previous)
  elif previous.is_symlink():previous.unlink();syncdir(base)
 for dst in reversed(created_config):
  if dst.is_file():dst.unlink();syncdir(dst.parent)
 for dst in reversed(created_links):
  if dst.is_symlink():dst.unlink();syncdir(dst.parent)
 if live:
  subprocess.run(['systemctl','daemon-reload'])
  for service in reversed(services):subprocess.run(['systemctl','start',service])
 print('Activation failed: '+str(error),file=sys.stderr);sys.exit(1)
