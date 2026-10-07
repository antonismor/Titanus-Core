# Replicated Realm control plane

Titanus embeds HashiCorp Raft and its BoltDB adapter in `titanusd`. This is a
native Go consensus library, not an external orchestrator or a separate service.
Unit runtime, Realm objects, transport authorization, placement and reconciliation
remain Titanus components. Dependencies and checksums are pinned in `go.mod` and
`go.sum`; Raft uses MPL-2.0 and the adapter MIT licenses.

## Authority and durability

An HA Realm has **3 or 5 static CONTROL voters**. Three voters tolerate one
unavailable controller; five tolerate two. Only the current quorum-backed leader
serves Realm operations. It commits a complete Realm replacement to the Raft log
and applies it after majority acknowledgement. The log/stable term/vote store uses
BoltDB with fsync enabled; snapshots include committed state and persist through
Raft's crash-safe snapshot store. Initialization fsyncs directory entries too.

Realm state includes Nodes/Pulses, Fabric configuration, Fleets and their complete
rollback histories, assignment leases and readiness, Routes and Network Policies.
An FSM revision comparison rejects stale proposals. Store mutations use an
isolated candidate and discard uncommitted state after rejection or validation
failure. A restarted voter restores snapshots/replays committed log state; the
original standalone `state.json` is an initialization seed, never the live HA
source of truth. Direct standalone writes and daemon downgrade without HA config
fail closed once the membership marker exists.

All Realm API reads and writes first verify leadership with quorum and an FSM
barrier. Followers/minority return HTTP 503 with `X-Titanus-Rejected: true` and
an informational `X-Titanus-Leader` hint. `GET /v1/realm/consensus` reports local
Raft status without claiming quorum health. `GET /v1/health` still reports daemon
process health; use an actual Realm request to test control-plane availability.
The reconciler checks quorum before a pass and before **each** remote node action.
Scheduling lease watchdogs remain on execution nodes.

A timeout or loss of leadership can mean a write's outcome is **unknown**. Read
committed state before an operator retries a Fleet revision change. An old log
entry may legitimately be committed after quorum returns. Never infer failure
from a missing response or automatically replay such an operation.

## Authenticated consensus transport

Raft listens on a separate TCP port, default **9444**, and requires TLS 1.3 with
the Realm CA and a `controller` certificate. Local identity must match its config;
outgoing certificate identity must match the selected peer; incoming identities
must belong to the configured voter set. Certificate SAN verification stays on.
An ALPN digest binds connections to the identical Realm, ordered peer set and
normalized initial seed. Incorrect Realm/membership/seed configurations cannot
silently form a cluster. Identity, initial membership and seed digest are also
bound to the persistent directory and checked before opening it.

Every read/write on a long-lived consensus stream rechecks certificate validity
and the signed CRL. Rotation reloads certificate/key generations through the
existing PKI bundle support. Closing the transport closes accepted and dialed
connections, including pooled/heartbeat connections, before a restart/rejoin.

## Fresh installation

`realm deploy` enables consensus automatically for plans with 3 or 5 CONTROL
nodes; two or other unsupported multi-controller counts are rejected. A single
CONTROL node keeps existing standalone behavior. `--cluster-port` selects the
API port; `--raft-port` selects the distinct consensus port. No floating VIP is
required for internal clients: each Agent and Gateway receives every configured
controller HTTPS origin.

Deployment stages `/etc/titanus/ha.json` and adds `TITANUS_HA_CONFIG` on **all**
CONTROL nodes. It seeds the same network configuration on each controller before
starting services. The designated primary bootstraps the initial voter set once.
Persisted Raft state prevents rebootstrap after restart. All controllers run a
reconciler; quorum guards allow only the elected leader to do work. Allow 9443
from nodes/management clients and 9444 **between configured controllers**.

Example local configuration for `control-1`:

```json
{
  "id": "control-1",
  "realm": "LAB",
  "bootstrap": true,
  "peers": [
    {"id":"control-1","address":"10.0.0.11:9444","api":"https://10.0.0.11:9443"},
    {"id":"control-2","address":"10.0.0.12:9444","api":"https://10.0.0.12:9443"},
    {"id":"control-3","address":"10.0.0.13:9444","api":"https://10.0.0.13:9443"}
  ]
}
```

Set `bootstrap` to false on the other two controllers and change only `id`.
Keep the **ordered** peers identical. The seed hash is calculated and persisted
on first startup; an operator may also pin it explicitly as `seed_hash`.
The daemon's `TITANUS_NODE_ID` and `TITANUS_REALM_NAME` must match the config.
Each voter needs its own private state directory and controller certificate.

Agent `--controller` accepts comma-separated HTTPS origins. It probes only
these configured origins and never follows an arbitrary server-supplied leader
URL. GET/HEAD and idempotent node registration/Pulse can fail over after a
transport error. A generational write retries only after an explicit rejection
before execution; ambiguous transport errors are returned to the caller. HTTPS
redirect following is disabled. Failed dial/handshake/header waits are bounded
per attempt. Dedicated Gateways fetch the current leader's state through the
same trusted endpoint set, including when their local controller is a follower.

Run `titanus realm consensus` on any controller for local consensus diagnostics.
Run `titanus realm status` and Fleet/Route/Policy management **on the elected
leader**: the privileged Unix CLI does not forward administrator authority using
a controller certificate. Follower errors include the trusted leader's API URL.

## Existing standalone Realm migration

Stop the old reconciler and daemons before migration. Export/copy the **same
complete** `realm/state.json` to each voter with mode 0600 and pre-stage identical
Source bundles on all controllers. Configure certificates and the peer list,
then bootstrap one designated voter and start the others. A first seed commit
starts the HA Realm revision sequence at 1 and preserves Fleet generations and
history. The seed hash ignores only top-level Revision/UpdatedAt; substantive
state differences reject transport authentication. Keep the original seed file
unchanged after initialization: restart checks its binding and recovers live
state from Raft. This is an operator migration procedure, not a live migration.

Do not delete an isolated voter's Raft data or reuse its identity with empty
storage. Restart it with the same configuration/log directory and let it catch
up. Do not force-bootstrap a minority; restore quorum. Dynamic add/remove of
voters, membership changes, automated recovery after permanent majority data
loss and force-recovery commands are intentionally not exposed in this version.

## Limits and verification

Complete Realm replacements are capped at **512 KiB**, matching the embedded
Raft library's suggested entry size. Oversized candidates fail before append.
This favors correctness for initial clusters; incremental commands and larger
cluster scalability remain future work. Every mutation incurs quorum disk/network
latency; no benchmark or production throughput guarantee is claimed.

**This is HA for Realm control state and scheduling authority.** Source payloads,
local Disks, runtime processes, CA signing keys/CRL issuance state and external
Route listener/VIP ownership are not replicated by this FSM. Deploy the same
Sources on each controller before failover to a new execution node. CA signing
stays on the designated primary; renewal clients can reach it through the
endpoint list while it is up. Leaf/CRL validity bounds how long a signer outage
can be tolerated. Do not copy signing keys to all voters as a substitute for a
replicated PKI lifecycle. Control plane quorum checks do not fence storage writers
or cancel a remote operation already in flight when leadership changes. Existing
acknowledged retirement and node lease-expiry rules still apply; storage fencing
remains a separate production milestone.

Tests use real loopback TLS sockets and private BoltDB logs for three voters:
leader partition/election, minority write rejection without state leakage,
rejoin, full restart, stale revision rejection, persisted rollout/rollback,
snapshot transfer to a lagging voter, role/identity rejection and actual mTLS API
failover. Race checks cover consensus, Store integration and client failover.
These execute on native AMD64 and ARM64 CI runners. They test separate voter
instances on one machine; separate-host reboot, WAN faults and production
installation are not claimed by this test suite.
