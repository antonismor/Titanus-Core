# Authenticated offline host backup and recovery

This is the first M4 checkpoint, introduced in `0.4.0-rc.5`. It backs up one
dedicated logical Node at a time, with its complete node-global UID/GID ledger.
It preserves the exact Realm/Node/role, paths, state profile and native
architecture. No service, workload, SSH connection or storage backend is started
by these commands. Full cluster disaster recovery remains M4 work.

## Supported inventory and prerequisites

The complete selected state tree includes standalone Realm state or all persisted
Raft membership/log/snapshots, committed shutdown export, Sources, stopped Unit
specifications/layers, local Disk and snapshot bytes, mapping/ownership records,
node catalogs and observations. The complete configuration tree includes role-
bound PKI, controller signing key, certificates/CRLs, service environment and
`secrets.json` when encrypted records/bindings require it. The ledger tree retains
**all allocations, including retired identities**. Authentication keys live
outside all three roots, and must be owned 0600 regular files of exactly 32 bytes.

Before backup, independently quiesce the whole Realm and every writer; stop Units,
detach their mounts, withdraw Gateways and gracefully stop all daemon/agent
services. Stop legacy processes too. rc.5 supported root CLI/daemon/agent and live
installer paths hold a shared host lock; backup/restore takes its exclusive
nonblocking lock. This lock does not freeze arbitrary programs, direct library
users, other namespaces, remote hosts or external storage clients. Process
inventory additionally refuses remaining named Titanus actors. The operator
remains responsible for a common quiescent cross-host point and exclusion of all
old actors/access paths. No automatic force-bootstrap or membership rewriting is
provided.

After graceful Raft shutdown and database close, the controller writes
`realm/consensus/offline.json` with committed applied state and database/membership
SHA256 bindings. Backup validates it and rejects any later database modification,
missing checkpoint, or command beyond/disagreeing with its exported revision.
A controller killed before that export must first recover through existing quorum
and be cleanly stopped. The old standalone `realm/state.json` is a seed in HA and
is never treated as the authoritative current state.

Only regular files, directories and symlinks are supported. Numeric UID/GID,
permissions (including special permission bits), mtime and symlink text are
authenticated. Links are never traversed during archive walking/extraction;
configuration links must resolve inside the archived configuration tree. Directory
symlink parents, mounted roots/subtrees, hard-linked files, extended attributes,
ACLs and special/whiteout/device files are refused. Source/data links preserve
their text, **not external target data**. Limits are 100,000 entries, 32 MiB signed
manifest and 1 TiB logical regular-file data; sparse files are streamed as logical
bytes. Configuration/ledger roots must be private and owned by the operator.

Every referenced secret key must authenticate/decrypt its records. Every committed
Source digest and retained Fleet/Task/Unit payload must exist. Live/starting/
desired-running Units, missing/conflicting Unit mappings, busy Disk locks, corrupt
inventories and any Ceph Disk/catalog are refused. Remote data cannot be recovered
from a metadata-only archive, so this profile does not create one.

## Plan and creation

Example `host-backup.json` (replace the Realm/Node/role with actual identities):

```json
{
  "format": "titanus-backup-plan/v1",
  "realm": "TITANUS-REALM",
  "node": "controller1",
  "role": "controller",
  "state_root": "/var/lib/titanus",
  "config_root": "/etc/titanus",
  "ledger_root": "/var/lib/titanus-userns"
}
```

All roots must be existing real, nonoverlapping absolute directories. Provision
an empty private ledger directory if this dedicated Node has never allocated a
mapping; otherwise preserve its actual full ledger. `role` is `controller` or
`node` and must match the archived certificate. The conventional `pki/node.crt`,
`pki/node.key`, `pki/ca.crt`, `pki/ca.crl` and `secrets.json` paths are validated;
controllers also require `pki/ca.key`, and HA requires `ha.json`. Custom external
key/configuration locations need an explicitly supported profile, not omission.

After independently performing the shutdown/exclusion procedure:

```sh
sudo titanus backup keygen --key /secure-backups/recovery.key
sudo titanus backup create --plan /secure-backups/host-backup.json \
  --key /secure-backups/recovery.key --archive /secure-backups/controller1.tar.gz
sudo titanus backup verify --key /secure-backups/recovery.key \
  --archive /secure-backups/controller1.tar.gz
```

The key/output parent must already exist. Existing key/archive outputs are never
replaced. A unique private temporary archive is fsynced and published without
replacement only after two identical complete inventories and file checksums.
The result reports backup ID, Realm/Node, applied revision, entry count, producer
build identity and `offline-host-local` scope; it never prints private contents.

HMAC-SHA256 authenticates the manifest, which binds each payload's path/type,
numeric metadata and SHA256. The recovery key is a separate operator trust
anchor: possession grants backup/attestation signing authority. This is not an
external signing service or KMS. **The archive is not encrypted and includes
private signing keys/keyrings.** Protect it with encrypted restricted backup
storage and preserve the recovery key independently.

## Recovery exclusion and offline restore

Recover only the same logical Node on a dedicated, independently fenced host,
at its exact original absolute paths, producer version and native architecture/profile. Do not
copy another Node's ledger or restore one independently of its data. Prepare
existing real parent directories; all three destination roots must be absent.
Commands refuse replacement of existing identities or data. They do not remove
live installations or prepare destinations automatically.

Independently prove/exclude old controllers, workloads/storage writers and Gateway
VIPs before recovery, including their ability to return after power/network
restoration. Record the external observations in a JSON record such as:

```json
{
  "realm": "TITANUS-REALM",
  "node": "controller1",
  "old_controllers_excluded": true,
  "old_writers_excluded": true,
  "gateways_withdrawn": true,
  "evidence": "Reference to the actual independent exclusion observations and enforced return prevention."
}
```

The sample describes fields, not actual fencing. `attest-fence` signs **the
operator's independently obtained record**; it does not perform power fencing
or measure whether the statements are true. The signed attestation is bound to
the verified archive hash/ID and logical identities and expires after 15 minutes:

```sh
sudo titanus backup attest-fence --key /secure-backups/recovery.key \
  --archive /secure-backups/controller1.tar.gz --record /secure-backups/exclusion.json \
  --output /secure-backups/controller1-fence.json
sudo titanus backup restore --plan /secure-backups/host-backup.json \
  --key /secure-backups/recovery.key --archive /secure-backups/controller1.tar.gz \
  --fence /secure-backups/controller1-fence.json
```

Restore authenticates the **whole** archive before touching targets. Fresh private
stages are populated without traversing links, checked for complete inventory,
identities, signing/keyring functionality, mapping consistency, Source integrity
and exported Realm revision/hash. Numeric metadata is restored without username
translation. Files/directories and parent rename entries are fsynced; Linux
no-replace renames publish configuration, ledger and state. No process starts.

Cross-directory publication cannot be one filesystem transaction. The durable
`STATE_ROOT.recovery-pending` marker is installed first and retained across every
error/crash. The `STATE_ROOT.recovery-receipt.json` records the archive/fencing,
prepared state and completed root renames. Supported daemon/agent/CLI/live
installer startup refuses a pending marker. Successful completion fsyncs its
receipt and removes the marker last. A consumed receipt prevents silent replay,
even if somebody later deletes the restored roots.

If interrupted, keep services/old actors excluded. Inspect the receipt and exact
three roots offline, verify their archive inventories/identities, and plan
explicit completion or complete disposal of the attempted recovery. There is no
automatic resume, cleanup of published roots, identity/data rollback or generic
"remove the blocker and start" operation. An unfinished backup similarly leaves
`STATE_ROOT.backup-pending`; its input state was not rewritten, but inspect the
offline state and temporary/output archive before clearing that marker.

Historical PKI policy is restored at its backup point. This does not recover
later revocations/issuance, extend expired certificates/CRLs or establish safe
mixed-version operation. Keep all restored services offline until identity
validity, exclusion, full cluster/storage recovery and membership have been
reviewed together. A full cluster cut/recovery-set protocol remains pending.

## Evidence and remaining M4 work

Default tests reconstruct functional Realm writes, TLS identity, decryptable
secrets, Source identity, stopped Unit catalog and writable local Disk data; they
reject corrupt content/metadata, wrong keys/plans/fencing, busy data and partial
restore replay. Both native architecture CI jobs additionally reconstruct three
Raft voters after loss of their original state/configuration/ledger trees and
original authority/Source inputs. They prove post-recovery quorum writes and PKI
policy signing, replicated secret decryption, Source identities and separate
local bytes. Numeric UID/GID/mode preservation and actual daemon refusal after
an unfinished recovery are mandatory in native CI. The voters share a runner
kernel; real power-loss/fsync semantics and independent-host clocks/network/boot
are not established by these tests.

M4 remains open for authenticated Ceph RBD/CephFS data export/import and catalog
rebinding, coordinated complete multi-host/controller recovery sets, measured
stale-writer exclusion and full controller/storage disaster recovery from
independent backups. M5–M7 remain independent milestones. All user VM/physical
host acceptance remains deferred to the central large laboratory server.
