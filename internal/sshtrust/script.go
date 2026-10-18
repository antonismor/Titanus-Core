package sshtrust

// The SSH account must be root (including a named UID-0 account). No daemon
// configuration is changed. Existing authorized_keys lines are retained.
const remotePython = `
import fcntl,json,os,pathlib,stat,subprocess,sys,tempfile
p=json.loads(sys.argv[1])
if os.getuid()!=0: raise RuntimeError('root account required')
home=pathlib.Path.home()
def directory(path):
    if path.is_symlink(): raise RuntimeError('symlink directory refused')
    path.mkdir(mode=0o700,exist_ok=True)
    s=path.stat()
    if not stat.S_ISDIR(s.st_mode) or s.st_uid!=os.getuid(): raise RuntimeError('unsafe directory')
    os.chmod(path,0o700)
def regular(path):
    s=path.lstat()
    if not stat.S_ISREG(s.st_mode) or s.st_uid!=os.getuid() or s.st_mode&0o077: raise RuntimeError('unsafe key file')
def atomic(path,data):
    if path.exists() or path.is_symlink(): regular(path)
    fd,name=tempfile.mkstemp(prefix='.trust-',dir=path.parent)
    try:
        with os.fdopen(fd,'w') as f:
            f.write(data);f.flush();os.fsync(f.fileno())
        os.replace(name,path)
    finally:
        if os.path.exists(name): os.unlink(name)
directory(home/'.ssh')
directory(home/'.ssh'/'titanus')
root=home/'.ssh'/'titanus'/p['id'];directory(root)
lock=os.open(root/'lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
with os.fdopen(lock,'w') as f:
    if os.fstat(f.fileno()).st_uid!=os.getuid(): raise RuntimeError('unsafe lock')
    fcntl.flock(f,fcntl.LOCK_EX)
    key=root/'id_ed25519'
    if key.is_symlink(): raise RuntimeError('symlink key refused')
    if not key.exists():
        # Refuse a dangling/stale public file rather than overwriting it.
        if pathlib.Path(str(key)+'.pub').exists() or pathlib.Path(str(key)+'.pub').is_symlink(): raise RuntimeError('orphan public key')
        subprocess.run(['ssh-keygen','-q','-t','ed25519','-N','','-f',str(key)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    regular(key)
    public=subprocess.check_output(['ssh-keygen','-y','-P','','-f',str(key)],text=True).strip()
    auth=home/'.ssh'/'authorized_keys'
    # Serialize with other Titanus bootstrap operations across Realm IDs.
    fd=os.open(home/'.ssh'/'titanus'/'authorized_keys.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as a:
        fcntl.flock(a,fcntl.LOCK_EX)
        if auth.is_symlink(): raise RuntimeError('symlink authorized_keys refused')
        if auth.exists():
            s=auth.stat()
            if not stat.S_ISREG(s.st_mode) or s.st_uid!=os.getuid() or s.st_mode&0o022: raise RuntimeError('unsafe authorized_keys')
            # Existing readable public-key files may be 0644; preserve entries.
            os.chmod(auth,0o600)
        content=auth.read_text() if auth.exists() else ''
        restriction='no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc '
        for k in [p['installer']]+(p['peers'] or []):
            blob=k.split()[1]
            if not any(blob in line.split() for line in content.splitlines() if not line.lstrip().startswith('#')):
                if content and not content.endswith('\n'): content+='\n'
                content+=restriction+k+' titanus-'+p['id']+'\n'
        atomic(auth,content)
    if p['known']: atomic(root/'known_hosts',p['known'])
    print(public)
`
