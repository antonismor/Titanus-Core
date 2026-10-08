# Phase 2 HA lifecycle

This extends CONTROL_PLANE_HA.md and STORAGE_FAILOVER.md. State profile v3
contains public PKI policy, immutable Source identities and Gateway epochs that
older binaries cannot preserve. Cross-profile installation/rollback is refused;
M5 owns verified migration. Independent-host acceptance remains pending in #20.

## Signing and certificate lifecycle

Privately provision the **same** Realm CA certificate/key on each controller
eligible to sign, using root-owned private files and secure offline transport.
Do not place `ca.key` in Realm state, a Source, an API payload or a public
artifact. Losing the former signing controller does not remove signing capacity
from surviving provisioned controllers. This is replicated signing authority
with private key provisioning, not threshold cryptography or an external KMS.

The active quorum leader imports the initial signed CRL once. Subsequent
renewal, revocation and CRL maintenance use the committed CA fingerprint,
issuance sequence and cumulative CRL; local stale copies cannot discard another
controller's revocations. A certificate/CRL response is served only after its
transaction is durably committed. Rejected/uncertain writes expose no signed
result. Followers/minorities reject signing and advertise the trusted leader.
Renewal preserves authenticated role, ID, SANs and usages; the replacement key
stays with the client. Serial allocation survives leader loss and restarts.

All controllers install committed, monotonically numbered signed CRLs. Each
request additionally checks committed revocation policy, including reused TLS
connections. Execution nodes retain the existing signed-CRL Pulse sync and
atomic leaf/key bundle renewal. Expired CA, leaf or CRL fails closed. An offline
node cannot infer a later revocation it has not fetched; keep leaf TTLs short
and allow timely Pulse policy delivery. Keep controller clocks synchronized.

Use authenticated `/v1/identity/renew` and administrator POST
`/v1/identity/crl` for live operations. Initial enrollment/offline CLI issuance
remains a private administrator operation. Once policy is committed, do not use
controller-local `pki revoke`/`MaintainCRL` to make live HA policy: those files are
caches of the quorum result. Root CA replacement is a coordinated offline trust
migration with backups and new enrollment; changing the pinned CA online is
deliberately rejected. Key loss and root migration recovery belong to M4/M5.

## Controller Source availability

An administrator can import a Source locally, then POST
`/v1/realm/sources/NAME`. Before committing its identity, the controller
replicates/verifies the payload on every configured voter. First use of an
unpublished Source in an HA Fleet/Task also performs this publication step;
all voters must be reachable for a **new** Source identity. Published Sources
continue to operate with a surviving quorum.

Identity covers regular bytes, names, directory/file modes, UID/GID ownership
and symlink text; timestamps do not affect identity. Names are immutable: publish
a new versioned name for changed content. Existing conflicting copies are never
silently overwritten. Execution-node HEAD probes verify identity rather than
presence alone. A new leader restores absent local payloads from trusted peers,
verifying the committed digest before atomic publication. Corrupt local copies
fail closed and require offline repair from a known-good replica.

Source bundles v2 verify the complete inventory/metadata and reject duplicate
entries, unlisted objects, URL-name mismatch and extraction through symlink
ancestors. Legacy v1 import remains available; new exports use v2. Import/export
to an older peer lacking metadata identity support is denied. Directory imports
and received bundles fsync staging before publication. Mutable administrator
edits underneath a published Source are detected, not incorporated as a new
version. Keep at least two verified replicas and back them up.

## Managed Gateway VIP ownership and failover

Opt in on each Gateway with `TITANUS_GATEWAY_MODE=true`,
`TITANUS_GATEWAY_VIP_MANAGEMENT=true`, a stable `TITANUS_NODE_ID`, and trusted
controller endpoints. The Gateway node must be registered READY with GATEWAY
capability. POST `/v1/realm/gateways/NAME` with `name`, IPv4 `/32` `vip`, `device`,
`owner`, and optional distinct `standby`. The Realm owns the epoch. Bind a Route
to that Gateway with `gateway: NAME` and `listen_ip` equal to its VIP. Legacy
unbound Routes retain fixed-listener behavior; they do not acquire managed VIPs.

Only the committed owner at a non-retired epoch can acquire the VIP/listeners.
Before acquisition, local ownership intent is fsynced. On startup, retained
addresses are withdrawn before reading live quorum state. Loss of state access
closes listeners and active proxy connections and attempts ordinary kernel VIP
withdrawal. A transfer commits an immutable intent first, then asks the exact old
owner/epoch to withdraw. Its durable retired-epoch record prevents a stale
snapshot or restart from reacquiring it. The old kernel must confirm address
absence before acknowledgement. Only then does a quorum commit change owner
and increment epoch. Failed/uncertain publication can retry the same withdrawal.

POST `/v1/realm/gateways/NAME/transfer` with `destination` performs a controlled
handoff. An unreachable owner with configured `standby` triggers reconciliation.
An API timeout, scheduling lease or missing daemon is **never** exclusion proof.
If ordinary withdrawal is unavailable, handoff stays pending unless an explicitly
configured out-of-band power fence confirms the old physical/VM node is off.

Optional `TITANUS_POWER_FENCING_CONFIG` points to a root-owned private JSON map
from registered node ID to a trusted target. Each target has `provider`, exact
`uuid`, and `address`. `ipmi` also has `user` and private `credential_file`; it
uses `/usr/bin/ipmitool` LANPlus/cipher 17, verifies the BMC system GUID before
power-off and again after two confirmed off observations. `libvirt` uses
`/usr/bin/virsh` with an explicit `qemu+ssh://.../system` or `qemu:///system`
hypervisor URI, verifies domain UUID, destroys that exact domain if running, and
confirms `shut off` twice. Quorum authority is checked before mutation and during
verification. Target responses/errors never log credentials. Missing config,
wrong UUID, unreachable management path or ambiguous status denies transfer.
These providers are opt-in code; no user's host is contacted by preparing it.

After a power fence, the entire old node is quarantined; Pulse and
re-registration cannot clear it. Re-enable only after offline reconciliation,
fresh enrollment/identity, verified current Realm state and removal of stale
VIP/Unit state. At most 128 immutable transfer records are retained. Static
membership, private signing keys, mapping ledgers and fencing target bindings
must survive recovery. Physical fencing covers the **whole node**; it is suitable
only for dedicated nodes where that scope is authorized by the operator.

Managed VIPs require the same reachable L2 segment/interface and appropriate
neighbor-cache convergence. Native CI verifies kernel address removal and
retired-epoch restart in isolated network namespaces; independent-host ARP,
routing, hardware power-off, hypervisor authentication, frozen processes,
partitions, power-on/rejoin and concurrent transfers remain final VM/host tests.

## Verification

Go/race tests cover minority/follower signing denial, cumulative revocations and
serial sequence through TLS Raft leader loss, Source integrity/extraction,
withdrawal persistence and failed power evidence. Native AMD64/ARM64 CI kills
real controller daemons, renews a leaf on the successor signer, checks retained
revocation, restores an absent controller Source from a surviving replica, and
checks real kernel VIP withdrawal plus restart with a stale epoch. Power-provider
unit tests exercise command/identity ordering using injected responses; actual
BMC/hypervisor execution is **deferred**. Checkpoint #20 records exact successful
heads/main runs/releases; this document alone does not claim CI or lab acceptance.
