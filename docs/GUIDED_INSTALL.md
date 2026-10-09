# Guided versioned lifecycle installation (plan v2)

`0.4.0-rc.4` extends the ANSI terminal wizard with **initial install, upgrade,
and binary rollback**. It prepares reviewed files locally by default. No SSH,
remote discovery, service operation or storage provisioning occurs while
answering the wizard or preparing a deployment.

## Requirements and workflow

Use dedicated Debian-family Linux hosts with native AMD64 or ARM64, cgroups v2,
OverlayFS, iproute2, nftables, util-linux/nsenter, Python 3, tar, sha256sum and
systemd. Provision addresses, routing, clock synchronization, SSH trust and
passwordless root/sudo first. The tool validates prerequisites; it does not
install host dependencies or configure interfaces. Use external IPv4 management
and VXLAN-underlay networks, outside the non-overlapping Unit Fabric and Service
CIDRs. Control membership is exactly 1, 3 or 5 voters.

1. Download **both native archives of the same published tested main revision**
   and obtain each SHA256 independently from its release checksum. Preserve the
   already published candidates. Do not use binaries from an uncommitted tree.
2. Run `titanus setup`. Enter node roles/addresses, optional Ceph networks and
   explicitly selected OSD devices, exact target release version and 40-character
   revision, both archive paths/hashes, API/Raft ports, node subnet size and
   VXLAN ID. The default final answer leaves host application disabled.
3. Review the saved `titanus-plan/v2` JSON and its adjacent private
   `.deployment` directory: `deployment.json`, ordered per-node `apply.sh`,
   initial `config.tar`, and `operator/` credentials. Preparation verifies full
   archive hashes, member inventory/checksums, manifest revision/version/state
   profile and all four ELF architectures without executing foreign binaries.
4. To prepare from a saved plan without contacting hosts:
   `titanus deploy release plan.json --output ./reviewed-deployment`.
   A new output directory is required; existing preparations are never replaced.
5. To authorize actual application, supply `--apply` with a **new** private
   output directory, or explicitly enable application in the wizard. This is
   the point that performs SSH preflight, transfers private files and changes
   hosts. Never run this step on user infrastructure before laboratory approval.

Relative archive and PKI paths are resolved against the invoking process's
working directory. Plans retain explicit Ceph `provision` settings; versioned
upgrade/rollback rejects Ceph provisioning. Initial provisioning requires
services to be started and only uses the explicitly selected devices. It is a
separate, destructive Ceph operation after Realm installation, not part of the
binary activation transaction. Pre-provisioned storage remains supported.

## Identity, roles and preserved state

Fresh preparation creates or reuses the named private Realm authority. An
existing authority for another Realm is rejected. All controllers receive the
protected CA key so M2 signing survives leader loss; workers receive only their
own node leaf/key, CA and CRL. Operator admin credentials stay in the private
local `operator/` directory. Keep the authority and these directories private;
all secret files are mode 0600 and preparation directories mode 0700. Leaf/admin
certificates have a 24-hour initial TTL: apply promptly and renew through the
existing authenticated PKI lifecycle. The tool does not print key material.

Initial configuration contains digest-bound environments, PKI and per-voter
membership. Only the first configured controller bootstraps; controllers are
installed before other roles and each receives the same network seed. Custom
API ports appear in both controller origins and advertised node endpoints.
Gateway capability is explicit; a managed VIP requires separate M2 ownership
registration and exclusion policy, and cannot be silently created by this plan.
Secret keyrings and existing Ceph credentials still require explicit protected
provisioning as documented in ORCHESTRATION.md and STORAGE_FAILOVER.md.

Initial activation refuses existing Realm state/configuration and custom flat
binary links. Configuration inventory and all destinations are checked before
writing, files are exclusive and fsynced, and ordinary activation exceptions
remove newly written configuration/links and restore selection. A process/power
interruption may leave partial initial files or seeded state: the next fresh
apply refuses to overwrite them. Inspect and recover explicitly; no automatic
crash/data recovery is claimed before M4/M5.

Upgrade/rollback retains `/etc/titanus`, shared state, identities, catalogs,
keyrings and workload processes. Every host is inspected before the first
transfer: installed Realm/node identity, native architecture and state profile
must match. All operations are serial, and a failure stops before the next host.
Completed nodes are retained in private `results.json`; earlier successful
nodes are not described as rolled back when a later host fails. Service-active
checks are operational checks, not application readiness or storage fencing.

Only **same `titanus-state/v3` profile** binary transitions are admitted.
`rc.1`/`rc.2` profile transitions and legacy flat provisioning need explicit
verified migration and remain refused pending M5. Binary rollback chooses the
actual previous installed release only when its exact version/revision match
the requested plan. It never rolls back data, membership or storage objects.
This installer does not yet negotiate mixed-version protocol capabilities or
implement availability-preserving multi-host rollout ordering; M5 and final lab
acceptance remain necessary before production upgrade claims.

## Verification and remaining acceptance

Native AMD64 and ARM64 CI verifies the real package/installer identity, staged
configuration/inventory rejection, upgrade and exact-target rollback, and a
live Unit retaining its PID across compatible activation. The guided fixture
verifies both archive architectures (the foreign archive is an explicitly
cross-built private fixture), installs three independently generated controller
configurations in staged roots, starts real daemons with separate bind/socket
paths and proves functional mTLS/admin access, quorum PKI and seeded network
state. All three generated controllers must answer authenticated consensus
requests with their exact identities, one common leader and two followers;
a functioning two-voter quorum alone cannot pass. Existing native Ceph and
failure suites remain mandatory.

These processes share a disposable runner kernel. Fresh heterogeneous VMs,
real SSH host application, interface/boot/power behavior, external VIP routing,
Ceph provisioning and mixed-version host upgrades are still deferred to the
central laboratory server. No user server is installed or started by repository
work or CI. Issue #20 remains open for M4–M7 and independent-host acceptance.
