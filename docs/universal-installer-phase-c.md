# Universal Installer — Phase C: storage HA and guarded profile transition

Phase C extends the Titanus-native Realm, Unit, Fleet, Pulse, Gateway and Disk
model. Community remains limited to nine participating hosts; Enterprise remains
**Soon Available**. Starting upgraded binaries does not convert an existing Realm
or advance its data schema. VM/physical-host acceptance is a separate final gate.

## Native HA Ceph policy

The optional Native HA storage profile requires at least three STORAGE hosts,
exactly three planned monitor members, replicated pools with **size 3 and
min_size 2**, and the `titanus-ha-host` CRUSH rule selecting distinct **host**
failure domains under `default`. Additional STORAGE hosts add OSD capacity without
changing monitor membership. CONTROL and EXECUTION remain separate as in Phase B;
STORAGE can be combined with either role. Counting logical hosts does not prove
independent hardware, power or network domains.

RBD uses the `titanus` pool. CephFS uses `titanusfs_meta` and `titanusfs_data`, and
requires an active MDS plus a standby on a different planned STORAGE host. RGW HA
is refused until its complete availability policy is implemented. Existing
Standard/Custom plans retain their earlier monitor and replication behavior.

Initial provisioning validates a preexisting CRUSH rule before assigning it,
sets the explicit pool policy and waits for a bounded healthy result. Completion
requires all three planned monitors at their planned addresses, an up OSD in
each planned host domain, `HEALTH_OK`, and every PG `active+clean`. A degraded
cluster can continue permitted I/O with two copies, but cannot authorize a new
HA profile activation. Runtime Disk writer exclusion and the separate confirmed
Worker power fence from Phase B remain mandatory before reassignment.

The transition probe is read-only and uses the controller's configured Ceph
client/keyring. Every retained Disk FSID must match that actual cluster. Retained
RBD/CephFS catalogs must use the explicitly verified pools/filesystem. It never
changes an existing cluster's pools, objects, devices or membership.

## Supported existing-Realm transition

The explicit `titanus ha` menu and its scriptable commands support existing
Standard/Custom **Realms with three or five static CONTROL voters**, five to nine
hosts, and at least two separate EXECUTION Workers in the target profile. Realm
name, every host identity/address, API/Raft endpoints, voter membership, Fabric
and Service networks are preserved. EXECUTION may be removed from CONTROL hosts
after workloads are quiesced; unrelated capabilities cannot change.

A one-controller installation cannot be converted by this path: adding voters
requires a separate coordinated membership migration. A ten-host installation
is refused without removing anything. Local/non-failover Disks, uncertain Tasks,
pending writer/Gateway fences, quarantined hosts and changing storage backends
also block conversion. This path preserves existing data and fails closed for
unsupported topology; it does not reinstall, reset Raft or manufacture a new
cluster identity.

## Operator sequence

These are commands for a separately scheduled maintenance window. Development
and CI do not execute them against deployed infrastructure.

1. Upgrade controllers and agents through the existing verified binary upgrade
   path to the same tested release/revision. Mixed AMD64/ARM64 hosts are supported.
   Retain the original profile/schema and review all host identities, fencing
   UUIDs, management credentials and physical failure domains.
2. Save Fleet desired settings in a durable maintenance journal; the existing
   `scripts/maintenance_transaction.py` preserves them. Quiesce all Fleets to
   zero, acknowledge every Unit retirement, terminalize Tasks, withdraw Gateways,
   detach mounts and independently exclude all writers. Gracefully stop every
   daemon/agent. No transition command force-stops a host or kills a workload.
3. Follow [complete coordinated recovery sets](BACKUP_RECOVERY.md)
   in `BACKUP_RECOVERY.md`: signed intent, offline non-voter cache alignment where
   required, all original host archives, authenticated Ceph data export and
   sealed complete set. Keep the private authentication key outside host roots.
   The original Ceph objects/FSID must be retained; this is not a Ceph cluster
   reconstruction image. Unsupported metadata/snapshot profiles are refused by
   the existing backup verifier.
4. Describe the target Native HA topology in a `titanus-plan/v2` plan. Keep the
   original addresses, ports, roles (except removal of CONTROL's EXECUTION),
   Fabric identity and exact tested build. Supply explicit UUID-bound fencing
   for Workers. The plan uses `operation: install` only as a target description;
   **do not submit it to the fresh installer**. Set Ceph `provision: false`.
   Existing Ceph HA configuration must already pass the read-only policy probe.
5. Prepare a new private artifact from the complete authenticated backup:

   ```sh
   sudo titanus ha prepare --plan /maintenance/native-ha-target.json \
     --backup-set /backup/set.json --key /backup/recovery.key \
     --output /maintenance/native-ha-prepared
   ```

   Preparation validates all archives, original membership and target topology;
   it changes no host. The artifact ID binds the target, backup revision/digest,
   host roots and private fencing-file hashes. Protect the whole output as root
   only. Keep controller fencing files and credentials off Worker packages.
6. On each stopped original host, make the authenticated recovery set available
   at its signed paths through the controlled offline recovery medium, then run:

   ```sh
   sudo titanus ha stage \
     --prepared /maintenance/native-ha-prepared/transition.json \
     --backup-set /backup/set.json --key /backup/recovery.key --node HOST_ID
   ```

   Staging re-verifies the full backup and exact signed host roots. A controller
   must still match the backed-up graceful Raft checkpoint. Protected service
   environments must match their authenticated archive bytes. A private durable
   journal records the preimage **before** any file changes. Only agent roles
   and the controller fencing environment/private configuration are staged;
   consensus databases, membership, Sources, secrets and Disk data are untouched.
   Interrupted writes can resume only when files match original or exact staged
   bytes. Conflicting operator edits are refused. A `staged` receipt is written
   only after all writes complete.
7. Restart the existing services through normal host orchestration, retaining
   the original quorum and zero desired Fleet instances. Verify every agent has
   a fresh READY Pulse with the target roles. The running controllers must have
   loaded the exact staged power-fencer inventory. Inspect local files with:

   ```sh
   sudo titanus ha readiness --id TRANSITION_ID
   ```

8. On the current leader's management socket, activate the reviewed artifact:

   ```sh
   sudo titanus ha activate \
     --prepared /maintenance/native-ha-prepared/transition.json \
     --socket /run/titanus/titanus.sock
   ```

   The existing original-membership schema protocol prepares durable rollback
   floors on **all voters and registered hosts**, then advances sequentially to
   schema 4. The admin activation API verifies authenticated host receipts,
   Node/Realm/build/role identity, fresh Pulses and read-only Ceph evidence before
   a revision-checked quorum commit. That commit records `NativeHA` and an
   immutable backup-bound activation receipt. A stale revision fails safely;
   retry the **same artifact** after inspecting current state. A lost successful
   response returns the same committed receipt without activating twice.
9. Verify the committed receipt and quorum convergence, then restore reviewed
   Fleet desired settings from the maintenance journal. Existing scheduling,
   leases, Worker quarantine, confirmed power fencing and remote-writer fencing
   govern future workloads. Capacity failback remains explicit and verified;
   there is no automatic power-on or relocation of healthy replacements.

## Rollback boundaries

Before activation has begun, `titanus ha rollback-staging --id TRANSITION_ID
--evidence 'DETAILED INDEPENDENT EXCLUSION EVIDENCE'` on each stopped host restores
only its saved configuration bytes. Set `TITANUS_STATE_ROOT` to that host's actual
state root when it differs from the default. Independently establish that no
quorum has begun activation. Operator edits are not overwritten. New private
files remain retained and unreferenced, together with the journal; nothing is
removed. Use a new preparation/artifact after a completed staging rollback.

Once schema-4 floors have been prepared, activation may have committed even if
one host has an old cache or the CLI lost its response. Configuration rollback
is therefore refused on **every host with that floor**, including non-voters.
An activated controller also refuses it from its committed offline state. Retain
the staged configuration and recover through the complete authenticated offline
cluster recovery protocol with independently established voter/writer exclusion.
Do not restore a single old voter into a live quorum, edit consensus state,
downgrade data, bypass floors or treat binary rollback as data rollback.

## Verification and remaining physical acceptance

Unit tests cover topology, retained identity, quiescence, artifact tampering,
CRUSH/pool/quorum/OSD/MDS evidence, activation revision/receipt immutability and
schema-four Worker fencing. The isolated native transition fixture uses five
real host identities, a real three-voter TLS quorum and authenticated host/data
archives. It verifies staged-byte conflicts, signed-root tampering rejection,
resume, byte-exact pre-activation rollback, real authenticated schema/activation
APIs, idempotence and post-activation rollback refusal. Source identities, signer
keys and original voter membership survive. Native HA seed backup/recovery also
checks that the genesis revision remains part of its consensus binding.

CI runs the transition on AMD64 and ARM64. The Ceph fixture runs three real
monitors and three native OSDs on sparse files in an isolated runner, verifies
host-domain replication, loses one monitor and OSD together, writes/reads data
through the remaining quorum/copies, then verifies clean rejoin and retained
bytes. Existing native RBD/CephFS writer-fencing tests remain in the five-job gate.
These fixtures share a kernel. Independent VM/physical host power loss, management
fencing, network partitions and site recovery/failback remain final laboratory
acceptance. No existing Proxmox host or VM is contacted by this change.
