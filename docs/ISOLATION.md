# Unit isolation — milestone 6

Every newly started Unit requires all of the following. There is no host-user,
privileged-device or disabled-LSM option. Kernel/setup/exec failure returns FAILED
and removes Fabric, cgroup and filesystem resources before reporting success.
Existing live Units must be stopped and recreated/restarted to take effect.

## Mapped identities

The monitor clones NEWUSER with NEWPID, NEWNS, NEWIPC, NEWUTS and NEWNET. Each Unit
has a 65536-ID UID/GID range; container UID/GID 0 is a nonzero host identity.
Run-as IDs must be 0–65535. The root-owned node-global ledger at
`/var/lib/titanus-userns/allocations.json` is locked and fsynced. Allocations are
monotonic, survive daemon/node restart and are never reused after deletion; a
new Unit incarnation gets a fresh allocation even with the same Unit name.
Reserve host IDs 1048576–2147483647 exclusively for Titanus on dedicated nodes:
no host accounts or other namespace allocator may use this range. Preserve this
ledger during backup/recovery. Do not copy a node ledger onto another node.

Each Unit gets a private ownership-shifted Source lower layer. Shared Sources are
unchanged. Symlinks are not followed; special files and owners outside the mapping
fail. This deliberately trades disk space for compatibility without requiring
idmapped OverlayFS. Source hardlinks/xattrs/file capabilities are not preserved
by the existing Source importer; no_new_privs blocks file privilege escalation.
Previously populated unshifted upper/work layers fail closed and require offline
migration or Unit recreation; there is no automatic recursive ownership rewrite
of existing data. The host state remains private; only an executable file handle crosses the startup boundary and closes on
workload exec. A temporary root-owned access directory exposes only the rootfs
behind a mapped-owner 0700 gate. Init resolves this path in its own mount
namespace, and the parent removes the access mount on startup completion/failure.
Host state directories and directory descriptors are not exposed. Init binds the
rootfs onto a private child staging mount (avoiding the inherited locked mount),
mounts its fresh PID proc while the inherited full proc is visible, pivots into
it and detaches the entire old root before masking proc and applying policy. The host monitor
alone writes durable exit records.

Writable Disks retain their real ownership. Provision them for the mapped UID/GID
shown by `unit inspect` (`state.user_mapping.base + run_as_uid/gid`) and appropriate
permissions. Titanus does not recursively chown shared or Ceph data. Existing
nonempty data requires an administrator's explicit migration. Storage attachment
ownership/fencing, snapshot/restore and real Ceph failover remain milestone 7.

## Devices and proc

A native eBPF BPF_CGROUP_DEVICE program is attached before init enters the Unit
cgroup. Stacked filters are ANDed. It allows only read/write of char 1:3, 1:5,
1:7, 1:8, 1:9 (null, zero, full, random, urandom), 5:0 (tty), 5:2 (ptmx), and
136:0–255 (private PTYs). Every block device, other character device and mknod
request is denied, including pre-existing nodes in mounted Disks. Filter load or
attachment failure aborts startup. Deleting the cgroup releases its filter.
The private /dev uses bind mounts of the safe devices because mknod is not
permitted by a mapped user namespace; devpts has a private, bounded instance.

The Unit mounts a fresh proc for its PID namespace. `/proc/sys`, `sysrq-trigger`,
`irq`, `bus` and `fs` are read-only; `kcore`, `keys`, `timer_list`, `interrupts`,
`sched_debug`, `kallsyms` and `slabinfo` are masked by empty read-only files when
present. Missing optional paths are accepted; other protection errors abort.
Disk targets cannot cover protected runtime paths or traverse Source symlinks.
Seccomp denies remount, setns, unshare, ptrace and namespace clone flags; capabilities
exclude SYS_ADMIN and MKNOD. This is layered protection, not arbitrary device
passthrough or permission to expose host proc/sys through storage.

## Explicit fail-closed LSM

`security.lsm: "landlock-v1"` is mandatory and defaults when omitted. It requires
Landlock ABI >=3, with all filesystem rights through REFER and TRUNCATE handled.
Missing/disabled Landlock, older ABI, invalid/missing rule paths, or failed
restriction prevents exec. The policy is applied on the locked exec thread just
before security narrowing and re-exec; new PID 1 and workload threads/descendants
inherit it. Pre-existing helper threads are discarded by exec.

The Unit filesystem is readable/executable. Writes are granted only to `/tmp`
by default and writable Disk targets. `security.lsm_write_paths` may explicitly
add existing directories such as `/var`; protected paths, `/`, noncanonical
paths and symlinks resolving to protected paths are rejected. Device creation is
never granted; safe /dev files allow ordinary I/O. Immutable rootfs and read-only
Disks remain read-only even where an LSM path otherwise grants writes.

This v1 policy does not claim Landlock ioctl, socket, signal or network mediation
introduced in newer ABIs. It does not replace AppArmor/SELinux administration;
those host policies may add restrictions and may prevent startup. The native
seccomp/capability and cgroup device controls remain mandatory. Allowed file
metadata/ioctl operations retain kernel permission checks.

Example Fleet/Unit policy (common JSON API, no privileged override):

```json
{
  "user_namespace": "mapped-v1",
  "devices": "safe-v1",
  "lsm": "landlock-v1",
  "lsm_write_paths": ["/tmp", "/var"],
  "seccomp": "titanus-default-v1",
  "no_new_privs": true,
  "run_as_uid": 1000,
  "run_as_gid": 1000
}
```

## Verification

Native AMD64/ARM64 CI runs the production monitor/init and kernel enforcement:
separate/stable mappings and host credentials, mapped persistent writers,
forbidden block/character nodes inside a real mounted Disk, safe device I/O,
read-only/masked proc, world-writable rootfs file and symlink LSM denial,
descendant inheritance, restart and LSM setup failure with no workload exec or
cgroup leak. Existing health/recovery, Fabric and rolling rollback tests remain.
Ordinary tests check exact device bytecode, policy downgrade rejection, durable
concurrent allocations/corruption and host mount/resolver symlink prevention.
No local privileged-kernel success is claimed when the development workspace
cannot grant mappings, capabilities or cgroup/BPF access.

References: [Landlock UAPI](https://docs.kernel.org/userspace-api/landlock.html),
[Linux user namespaces](https://man7.org/linux/man-pages/man7/user_namespaces.7.html),
[Linux cgroup device BPF](https://docs.kernel.org/admin-guide/cgroup-v2.html#device-controller).
