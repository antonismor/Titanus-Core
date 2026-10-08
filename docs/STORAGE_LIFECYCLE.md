# Disk lifecycle and attachment ownership

Titanus owns its Disk adapter and invokes the public Ceph CLI. It does not run
Docker, containerd, Podman or Kubernetes. The native CI cluster starts real
MON/MGR/OSD/MDS daemons directly on AMD64 and ARM64 runners.

## Attachment invariant

Every Disk, including read-only mounts, currently has one attachment at a time.
The runtime acquires a stable empty, root-owned, read-only flock descriptor before
binding data. Both the host monitor and namespace PID 1 retain that open file
description; the application receives none. A daemon or monitor crash therefore
cannot release a live writer. PID 1 exit destroys remaining namespace processes.
Owner records (Unit, incarnation, boot and random token) are diagnostic metadata;
they are never used as evidence that a stale writer is fenced. No time-based
scheduling lease, missing daemon or deleted owner JSON can release ownership.

Local Disks use node-local ownership. They cannot fail over to another node by
copying their JSON specification. RBD requires the `exclusive-lock` image feature
and maps with `--exclusive`, `noshare`, `lock_on_read` and a bounded lock timeout.
Cooperative lock transitions are disabled. CephFS uses MDS-mediated flock and
mounts with `noshare,recover_session=no`; both MDS blocklisting policies must be
true. An evicted client cannot silently reconnect its old mount.

Remote specifications and Ceph configuration must be pre-staged on participating
nodes. `data/` is exported to Units; CephFS `.titanus-control/` remains outside
that bind mount. Existing remote Disks with layout version 0 are rejected until
explicit offline migration. Existing local Disk paths remain compatible.

## Snapshots and restore

Stop every attached Unit first. Snapshot acquires maintenance ownership; a live
attachment rejects snapshots, detach and deletion, even after daemon restart.
Local snapshots are separate `cp -a --reflink=auto` copies preserving ownership,
xattrs, symlinks, hardlinks and sparse files, with fsynced publication. Ceph RBD
uses native snapshots of a quiesced, unmounted filesystem. CephFS uses native
subvolume snapshots. This is offline filesystem consistency, not application
transaction consistency: shut down or quiesce the application appropriately.

Restore always creates a **new** Disk and refuses an existing target. Local
restore copies the immutable snapshot, RBD deep-copies the selected snapshot,
and CephFS clones the subvolume snapshot and waits for `complete` before publishing
metadata. Pending/failed clones never become usable Titanus Disks. A remote
operation that succeeded but failed before local publication can leave an
orphaned remote object: inspect and reconcile it explicitly; do not retry by
silently overwriting remote data. Source snapshots remain intact.

```bash
titanus disk snapshot db before-upgrade
titanus disk snapshots db
titanus disk restore db before-upgrade db-recovered
titanus disk owner new-empty-disk HOST_UID HOST_GID
titanus disk detach db
```

`owner` only provisions an empty, offline data root. It never recursively rewrites
existing files. Preserve the UID/GID mapping ledger from the isolation milestone;
cross-node recovery must provision the same mapped identities or perform an
explicit offline ownership migration. No allocation mismatch is fixed by giving
all applications world-write permissions.

## Fencing and failover

Ordinary transfer requires an acknowledged stop and explicit remote detach.
An unreachable writable assignment stays blocked in the controller. The
controller does not mistake an API timeout or storage lease expiry for a fence.
RBD's non-cooperative exclusive kernel mapping rejects a second live writer;
Ceph handles stale exclusive-lock holders by blocklisting before lock takeover.
Never substitute an advisory `rbd lock add/rm` for the native exclusive lock.

CephFS manual fencing requires the current session ID, exact instance address
(including nonce) and matching subvolume root. An admin obtains these from:

```bash
ceph tell mds.FILESYSTEM:0 client ls --format json
titanus disk fence DISK SESSION_ID OBSERVED_ADDRESS
```

The adapter verifies both blocklist-on-eviction and blocklist-on-timeout settings,
then requests MDS eviction of that exact client. MDS's OSD-map epoch barrier
prevents the old client's writes before another client can acquire the lock.
Do not remove the blocklist entry or remount the fenced node until its old Units
are stopped and its runtime state is reconciled. Fencing one kernel session
fences every operation using that session; `noshare` limits the blast radius.
This first profile supports one active MDS rank (rank 0) for manual session lookup.
Automatic storage fencing and automatic rescheduling of unreachable writable
assignments are not enabled by this milestone.

## API and verification

`/v1/node/disks` lists/creates Disks. Object routes provide inspection, snapshots,
snapshot creation, restore, ownership provisioning, detach, fence and deletion.
Storage mutations require the existing CA-signed **admin** role. Controllers can
inspect; Node identities cannot administer storage. These are node-local APIs;
Disk specifications and snapshot catalogs are not part of the replicated Realm
FSM. Remote Ceph remains the authority for native objects and their exclusion.

The CI suite exercises native RBD/CephFS snapshots and independent restore,
remote competing-writer denial, acknowledged node-to-node transfer, and actual
CephFS eviction with a still-open synchronous stale writer. Runtime tests kill
the host monitor, recover a real mapped Unit, retain its attachment exclusion,
then verify release and restore after stop. CI must pass on the exact final PR
head before this scope is considered verified.
