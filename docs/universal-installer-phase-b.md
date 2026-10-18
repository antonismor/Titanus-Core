# Universal Installer — Phase B: Titanus Native HA

Phase B is stacked on Phase A (#46) and the Community edition changes (#45).
The existing Titanus Realm, Pulse, Unit, Fleet, Gateway and native storage model
is retained. No external orchestration platform or new workload abstraction is
introduced. This change does not access any deployed infrastructure.

## Profiles and compatibility

- Standard/Custom and legacy plans keep their current behavior. Community still
  accepts up to nine participating hosts. Enterprise remains **Soon Available**.
- Native HA initial installation accepts **5–9 hosts**: three or five CONTROL
  voters plus at least two separate EXECUTION Workers. CONTROL and EXECUTION
  cannot be combined in this profile. This keeps automatic Worker power fencing
  separate from the voting quorum.
- Every Worker requires a unique UUID-bound out-of-band fence. Supported
  providers are the existing IPMI, libvirt and HTTPS Proxmox providers. Provider
  availability and management access remain deployment prerequisites. Physical
  storage failure domains and redundant network paths must be designed by the
  operator; counting host nodes cannot establish hardware independence.
- Native HA is an explicit new-install profile. Selecting upgrade or rollback
  with this profile is rejected. Existing installations are not converted.
  Ordinary verified binary upgrade/rollback flows remain available under their
  existing profiles, subject to the installed schema compatibility floor.
- Schema 0/1/2 remain readable and writable. Only an empty offline Native HA seed
  advances to schema 3. Old binaries cannot join schema 3; private rollback
  floors prevent incompatible binary activation. No deployed schema is advanced
  by starting the new binaries.

## Prepare a new installation

Select `Titanus Native HA Cluster` in the wizard. Enter the topology and common
SSH details as in Phase A. Supply a **root-owned 0600** local fencing inventory:

```json
{
  "worker01": {
    "provider": "ipmi",
    "uuid": "00000000-0000-0000-0000-000000000001",
    "address": "192.0.2.101",
    "user": "titanus-fencer",
    "credential_file": "/root/titanus-secrets/worker01.password"
  },
  "worker02": {
    "provider": "ipmi",
    "uuid": "00000000-0000-0000-0000-000000000002",
    "address": "192.0.2.102",
    "user": "titanus-fencer",
    "credential_file": "/root/titanus-secrets/worker02.password"
  }
}
```

These are documentation-only example addresses and UUIDs. Supply real target
identities from independently verified management interfaces. Credential and CA
paths must be absolute. Do not commit credentials or a prepared deployment to a
repository. libvirt requires pre-established unattended management SSH trust;
IPMI controllers require `/usr/bin/ipmitool`, libvirt requires `/usr/bin/virsh`.
The release preflight checks the selected provider binaries on all controllers.

Planning validates inventory syntax without contacting management interfaces.
Preparing a release reads only private regular files, rejects symlinks, and packs
credentials into checksum-bound 0600 configuration archives on controllers only.
No secret value appears in a plan or generated shell. Non-controller nodes do
not receive the CA signing key or fencing configuration. Phase A's approved
fingerprints and mandatory directed SSH full mesh remain required by the wizard
apply path. Preparing alone does not install or fence anything.

Each new host receives the same Native HA policy and schema seed. Initial
migration receipts use a canonical genesis epoch; they are initial schema
metadata, not online migration timestamps. The consensus transport binds the
exact canonical seed. Only a pristine replicated state machine can accept that
prepared genesis; live schemas still advance one guarded step at a time.

## Automatic Worker failover

1. Pulse health marks an EXECUTION Worker UNREACHABLE. A SUSPECT Worker is not
   power-fenced. The controller requires current quorum leadership.
2. Before calling a management interface, commit a unique Worker exclusion
   intent and persistent power quarantine. The intent retains the exact old
   assignment incarnations, node identity, address and join identity.
3. The existing native fencer verifies UUID, requests power-off where needed,
   observes two off confirmations, rechecks identity and proves leadership
   around its operations. A missed heartbeat, API error or expired lease alone
   does not satisfy the Native HA Worker fence.
4. Commit completion only after confirmed physical exclusion. A crash before
   completion leaves a resumable intent; the next majority leader re-verifies
   the fence. Power management runs independently of Fleet orchestration, including slow
   or hung management interfaces. A failed target blocks its own handoffs while
   healthy Fleets keep receiving dispatch leases.
5. Retire old assignments only when the exact captured incarnation is fenced.
   Dispatch lease expiry and the existing safety margin still apply. Remote
   mounts additionally require registered automatic-failover catalogs and exact
   writer fencing through the existing Ceph RBD/CephFS mechanism. Retained
   storage fences continue to be refreshed. Local or unidentified writable
   storage remains blocked: it is never silently copied to a replacement host.
6. After acknowledged retirement, Titanus places the replacement using its
   existing Fleet placement rules and verifies runtime readiness. Available
   capacity and an accessible, published Source are still required.

The tests exercise stateless Worker replacement and blocked local storage.
Existing native Ceph tests cover real storage exclusion; a native HA hardware
acceptance exercise remains necessary for each real management/fault domain.
One-shot Tasks keep their existing uncertain-outcome behavior and are not
silently replayed as Fleet replacements.

## Safe failback / returning Worker

A returned heartbeat or registration **cannot clear quarantine**. Active
replacement assignments remain on their current hosts; Phase B never moves a
healthy application back merely because its former host returned.

For a fenced Worker to return as usable capacity:

1. Independently confirm the original machine identity, stale Unit exclusion,
   removal of old runtime entries and old mounts, and completion of all storage
   fences. Bring back that same Worker through the normal host recovery process.
2. Wait for a fresh READY Pulse. No pending storage or Gateway transfer and no
   remaining Realm assignments/Gateway references may refer to the Worker.
3. Submit an admin-authenticated request to the existing endpoint
   `POST /v1/realm/nodes/recover-power` with `node` and detailed `evidence`.
   The controller additionally probes the returning runtime over mTLS, verifies
   Node/Realm/schema identity and requires an **empty, strict runtime Unit inventory**, including refusal of
   unreadable, corrupt or redirected entries.
   Failed probes, old schema support, stale Pulse and retained runtime entries
   refuse recovery. The supplied evidence must describe independent stale-writer
   and mount exclusion; an empty runtime listing alone does not prove it.
4. The quorum commits a recovery receipt and clears quarantine. Only then can
   new placement use the Worker. A later failure requires a new fence intent;
   the historical completed receipt cannot authorize a new incarnation.

This is deliberate, verified capacity failback. Automatic power-on, automatic
quarantine clearing and automatic application relocation are not enabled.
No general maintenance cleanup is performed by this API.

## Verification

Coverage includes plan topology and fence identity rejection, offline TUI
saving, private credential packaging, exact assignment binding, interruption and
restart, failed physical fence, rejected quorum commit, returning Pulse,
explicit recovery, local-storage refusal, healthy-Fleet lease continuity and
real TLS/quorum leader isolation. Native guided-install CI now covers both the
existing three-controller profile and new five-host Native HA packages/schema
seeding on AMD64 and ARM64. All fixtures are isolated CI environments.

Required gate remains all **five** CI jobs at the current PR head: Go tests/vet/
race/cross-build, two native runtime jobs and two native Ceph jobs. No merge until
all five succeed. Merge dependencies in order #45, #46, then Phase B, retargeting
and re-running the complete checks at each step. Physical acceptance on the
user's infrastructure is outside this GitHub-only change.
