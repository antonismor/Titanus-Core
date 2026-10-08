# Phase 2: remote storage failover and catalogs

This extends STORAGE_LIFECYCLE.md. Legacy/unregistered Disks retain its manual
fencing requirements. No scheduler deadline, daemon absence or diagnostic owner
file is evidence of exclusion. Local Disks remain pinned to their actual node.

## Register offline, then execute

GET `/v1/node/disks/NAME/catalog` reads actual initialized Disk metadata, native
backend identity (Ceph FSID plus RBD image ID or CephFS subvolume UUID path), and
actual snapshot names. Set `fleet`, `auto_failover`, and `local_node` for local
Disks, then POST the complete object to `/v1/realm/disks` as an administrator.
Controllers validate remote identities and snapshot inventory against their own
privately provisioned Ceph configuration. Registration/refresh is only allowed
with no assignments for that Fleet. Identity, owning Fleet, local node and
failover policy are immutable; snapshot inventory may be refreshed offline.
Remote data is never created/formatted by adoption. Every controller and
participating node needs matching private Ceph configuration/credentials.

A catalog-bound Disk belongs to one Fleet with at most one instance. Node-local
realization copies only verified remote metadata and refuses mismatched or reused
native objects. Initialized legacy mounts must be detached before adoption.
Local catalogs pin placement; copying local JSON never migrates local bytes or
local snapshots. Unregistered Disks remain compatible with previous workflows.

Remote ownership follows the immutable managed Disk set across handoff AND
rolling generations. The Realm commits a 65536-ID allocation in the high half
of the reserved host mapping range (1073741824 through 2147483647); node-global
ledgers reserve exactly that mapping and reject any retained identity conflict.
The lower half remains for node-local Units. Private Source ownership shifting
and namespace mappings use this allocation. Provision a new empty Disk's data
root to the committed base (plus the application's relative UID/GID) before use;
existing populated ownership needs explicit offline migration. Changing a
Fleet's managed Disk set changes its ownership identity; no recursive ownership
rewrite or migration is performed automatically. Preserve BOTH Realm mapping
records and every node-global ledger. Only one Realm owns these pools per node.

## Automatic unreachable-writer handoff

1. Acquisition records the exact newly observed native client instance before
   exporting the mount. Reused mounts must have the same fsynced instance record.
   Ambiguous before/after session/watcher evidence denies export. The controller
   commits this writer evidence with the exact assignment/token/generation.
2. The node must be unreachable and the dispatch lease actually expired plus its
   safety interval. The controller commits an immutable failover intent containing
   every mounted Disk/catalog/writer and quarantines the old node in the same
   quorum write. Unknown writers, local/unregistered Disks, absent catalogs and
   changed objects remain blocked. A crash before writer evidence publication
   can require manual fencing; it never permits speculative replay.
3. With current quorum authority before each mutation, fence the exact instance
   in Ceph's OSD blocklist and verify every registered OSD has applied it. CephFS
   also evicts the exact recorded MDS session; unexpected other sessions deny
   automatic fencing. RBD's mandatory native non-cooperative exclusive lock
   handles lock takeover only after confirmed blocklisting. One active MDS rank
   is supported. A down/unreachable OSD fails closed.
4. The old instance is quarantined with a 10-year blocklist interval, renewed every day from immutable
   committed intents; no automatic
   unblock/remount occurs. Only after ALL Disks' native fences succeed does a
   quorum commit mark the old assignment stopped. A successor can be planned
   on a subsequent reconciliation pass. Intermediate success without result
   publication remains blocked and is revalidated idempotently after restart.
5. Acknowledged reachable cleanup stops/revokes the Unit and explicitly releases
   registered remote mounts before retirement. A fenced client's ordinary,
   non-lazy detach may proceed only with positively applied blocklist evidence.
   Pulse or node re-registration cannot clear storage quarantine.

The node's **entire** scheduling eligibility is quarantined. There is no automatic
quarantine-clear API in this candidate: stop/delete old Units, verify stale mounts
and mapping state, detach/reconcile, and rebuild the node identity/configuration
under a reviewed offline recovery procedure. Never delete blocklists or forcibly
break RADOS lifecycle guards to bypass a failed handoff. A crashed RBD maintenance
guard remains an explicit operator recovery condition. At most 128 immutable
failover intents are retained; reaching the limit denies new automatic handoffs.
Ceph stores expiry seconds in a 32-bit native timestamp. The duration is checked
before mutation and refuses overflow; indefinite controller downtime beyond
the retained fence expiry requires physical exclusion before storage access.
Fence refresh never discovers/evicts a successor. Failed renewal blocks that
reconciliation pass and remains due for retry. Catalog snapshot refresh and
intent archival are offline administrative work. Retained intents must never be
removed while an old client can return or before a separate permanent fence.

## Validation and remaining acceptance

Go/race tests cover durable intents, restart, unknown writer/live lease denial,
failed fence retention, immutable catalog ownership, pinned local placement,
quorum gating, mapping conflicts and preservation. Native AMD64/ARM64 Ceph CI
adds actual namespace/cgroup workloads on two independent state roots, a stalled
old watchdog, an open synchronous stale writer, a real RBD/CephFS fence, injected
controller loss after fencing before commit, restart/revalidation, successor data
integrity and a subsequent rolling generation retaining mapped file ownership.
The canary's file and parent directory are explicitly fsynced before simulated
loss. Unacknowledged application writes in the old kernel's dirty page cache are
outside crash durability guarantees; exclusion must prevent that kernel from
flushing them into the successor's filesystem after fencing.
These roots/processes share one runner kernel and native Ceph fixture.

Independent VM kernels, power loss, multiple OSD hosts, MDS failover, isolated
network partitions, timing/clock skew and real hardware remain **deferred** to
the user's central laboratory server. This document is implementation scope;
checkpoint issue #20 records which exact PR/main CI runs have actually passed.
No user-server installation or final laboratory acceptance is implied.
