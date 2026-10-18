# TITANUS Core

**Development And Design by [antonios.mortos@outlook.com](mailto:antonios.mortos@outlook.com)**

> **A new Linux-native application, container, cluster and distributed-storage platform designed around simplicity, automation and a unified user experience.**

Titanus is an independent infrastructure platform being built from first principles. Its goal is to combine application isolation, multi-node orchestration, cluster networking, persistent storage, recovery, scheduling and guided deployment in one coherent ecosystem.

Titanus is **not a Docker wrapper, not a Kubernetes distribution, and not a re-skinned existing orchestrator**. The Titanus runtime, control logic, object model, planner, placement engine, setup experience and APIs are designed as Titanus components.

The project is intentionally ambitious: instead of requiring an administrator to assemble a container runtime, orchestration layer, networking layer, storage integration and multiple operational tools, Titanus aims to provide one integrated platform that is easier to understand and operate.

## Vision

Docker made application containers practical. Kubernetes made large-scale orchestration practical.

**Titanus is being designed to go beyond that traditional split** by bringing runtime, orchestration, storage planning, cluster lifecycle and an interactive administrator experience into a single platform.

The objective is not API compatibility or source compatibility with Docker or Kubernetes. The objective is a clean platform with its own identity and a more approachable operational model.

### Core design goals

- **Titanus-native runtime** built on Linux kernel primitives.
- **Multi-node Realm architecture** for clustered operation.
- **Guided ANSI/TUI setup** for users who do not want to manually assemble infrastructure.
- **Automatic planning** before deployment.
- **Ceph integration** for distributed block, shared and object storage.
- **Native health and recovery logic**.
- **Placement engine** aware of CPU, RAM, storage, GPU and failure domains.
- **Cluster Fabric** for node and workload networking.
- **Human-friendly administration** from CLI, TUI and eventually Web UI.
- **Safe-by-default deployment** with discovery, validation, simulation and confirmation.
- **No hidden Docker, containerd, Podman or Kubernetes backend.**

## Community & Enterprise editions

**Community Edition — up to 9 Realm nodes.** A *node* is one participating VM
or physical host, regardless of its capabilities (CONTROL, EXECUTION, STORAGE,
GATEWAY, etc.). A VM with multiple roles still counts as **one node**.
The nine-node ceiling applies equally to Standard, Native HA and
Advanced/Custom installation profiles. The number of Units, Fleets,
applications or containers running on those nodes **does not count toward
the node ceiling**; practical workload capacity depends on CPU, memory,
networking and storage resources.

The Community installer and plan validator reject configurations with more
than nine Realm nodes. The Universal Installer candidate includes Standard and
Custom profiles plus approved, directed full-mesh SSH trust bootstrap
([Phase A](docs/universal-installer-phase-a.md)). The opt-in Native HA profile
uses 5–9 hosts, separate CONTROL voters/EXECUTION Workers, confirmed out-of-band
power fencing and explicit verified capacity failback
([Phase B](docs/universal-installer-phase-b.md)). Existing installations are not
automatically converted; local storage is never silently moved during failover.
[Phase C](docs/universal-installer-phase-c.md) adds the optional three-monitor,
three-copy host-domain Ceph HA policy and an authenticated, backup-bound profile
transition for existing 3/5-voter Realms with 5–9 hosts. Staging preserves original
membership/data; explicit quorum activation and guarded rollback are separate
steps. Single-controller membership conversion remains unsupported.
Enterprise entitlements remain unimplemented. See
[Editions and roadmap](docs/EDITIONS.md).

**Enterprise Edition — Soon Available.** No edition-imposed Realm node limit
is planned, subject to available infrastructure and validated scalability.
A **90-day trial** is planned for the Enterprise launch. No release date,
trial activation, download or licensing mechanism is available yet.
See [Editions and roadmap](docs/EDITIONS.md).

## Native Titanus terminology

Titanus deliberately uses its own object model:

| Titanus object | Purpose |
|---|---|
| **Realm** | Complete Titanus environment / cluster |
| **Node** | Physical or virtual Linux host participating in a Realm |
| **Unit** | Smallest isolated executable workload |
| **Fleet** | Managed group of related Units |
| **Source** | Filesystem/application blueprint used to construct a Unit |
| **Fabric** | Realm networking and workload connectivity |
| **Route** | Traffic exposure and load distribution |
| **Disk** | Persistent storage object |
| **Vault** | Secrets and protected configuration |
| **Profile** | Reusable resource and policy definition |
| **Pulse** | Health/state signal emitted by nodes and workloads |
| **Task** | Finite or scheduled operation |
| **Plan** | Validated execution plan generated before infrastructure changes |

## Architecture direction

```text
                         TITANUS REALM

                              │
                        Realm Core
                              │
          ┌───────────────────┼───────────────────┐
          │                   │                   │
     State Engine         Planner          Health Engine
          │                   │                   │
          ├──────────── Placement Engine ─────────┤
          │                   │                   │
          └──────────── Recovery Engine ──────────┘
                              │
                         Titanus Fabric
                              │
            ┌─────────────────┼─────────────────┐
            │                 │                 │
          Node A            Node B            Node C
            │                 │                 │
        titanusd          titanusd          titanusd
            │                 │                 │
          Units             Units             Units
            │                 │                 │
            └──────── Titanus Storage ──────────┘
                              │
                     Ceph / local providers
```

## Runtime direction

Native Unit starts enforce `no_new_privs`, a seccomp allowlist and zero Linux
capabilities by default. Explicit application capabilities are supported in
Unit and Fleet specifications. See [Runtime security](docs/RUNTIME_SECURITY.md)
for the policy, compatibility requirements, tested scope and remaining boundaries.

The Titanus runtime is intended to use Linux primitives directly:

- Linux namespaces
- cgroups v2
- OverlayFS
- `pivot_root`
- veth interfaces
- Linux bridges
- nftables
- Linux capabilities
- seccomp
- Unix domain sockets
- systemd integration

These are operating-system building blocks, not an embedded third-party container engine.

## Cluster design

A Titanus Realm can contain nodes with different capabilities:

```text
CONTROL
EXECUTION
STORAGE
GATEWAY
GPU
BACKUP
```

A node may provide one or many capabilities. Titanus does not require a rigid master/worker naming model.

## Ceph integration

Ceph is treated as an external distributed storage system accessed through a Titanus-owned storage adapter.

Planned storage modes:

- **Ceph RBD** for dedicated block-backed Titanus Disks.
- **CephFS** for shared filesystem-backed Titanus Disks.
- **Ceph RGW** for object storage use cases.
- Local storage providers for single-node and non-distributed environments.

The setup workflow is designed to ask the administrator what they need rather than requiring them to understand every Ceph parameter first.

## Guided setup

Run:

```bash
titanus setup
```

The ANSI interface collects Realm topology, management/Fabric and optional Ceph
addresses, capabilities, SSH access, API/Raft ports and exact release identities.
It supports initial installation, same-profile upgrade and pinned binary rollback
through a validated `titanus-plan/v2` plan. Preparation verifies both native
archives and writes an adjacent private `.deployment` directory for review.

Preparation is offline by default. Host application requires explicitly enabling
it in the wizard or using `titanus deploy release plan.json --output ./deployment --apply`
with a new output directory. That action performs SSH preflight before
transfer, applies nodes serially and records partial completion. Initial operator
admin credentials remain in the private local `operator/` directory; they are
not installed on workers. See [Guided lifecycle installation](docs/GUIDED_INSTALL.md)
for the review workflow, credential handling and transition limits.

Gateway VIP ownership is registered separately with its fencing policy. Ceph
provisioning requires explicit initial-plan approval and selected devices;
upgrade/rollback refuses provisioning. Independent VM/host application remains
deferred to final laboratory acceptance.

## Current repository stage

**Titanus Core M1-M7 are implemented and native-CI verified. The current published release candidate is `v0.4.0-rc.10`.**

The repository includes OverlayFS Units, cgroups v2, mapped Linux namespaces,
mandatory device/proc/Landlock isolation, bridge/veth/VXLAN/nftables Fabric,
service routing/DNS, signed mTLS roles, durable quorum control state, health
probes, readiness-aware rolling updates/rollback, native Ceph storage lifecycle,
authenticated observations, CPU autoscaling, durable Tasks, encrypted versioned
secrets and an authenticated Command Center.

All Phase-2 milestones **M1 through M7** are complete within their documented,
bounded implemented/native profiles. Native AMD64/ARM64 runtime and real Ceph
failure-path validation have passed the required five-job CI gates through
`v0.4.0-rc.10`.

The only remaining Phase-2 acceptance work is the **independent VM/physical-host
laboratory matrix** tracked in [issue #20](https://github.com/antonismor/Titanus-Core/issues/20):
kernel/clock/network/boot/power failures, stale-writer and storage partitions,
external Gateway/VIP failover, fresh install and mixed-version upgrade/rollback,
whole-quorum plus RBD/CephFS disaster recovery, sustained load/alerts/secret
rotation/autoscaling/Tasks, benchmarks and retained raw evidence.

These independent-host tests are intentionally separate from repository CI and
have not been claimed as complete. No repository CI result implies installation
or changes on user servers. Until that laboratory acceptance is complete,
Titanus should not be treated as a production replacement for mature container
or orchestration platforms.

## Build

```bash
make build
./bin/titanus setup
```

## Repository layout

```text
Titanus-Core/
├── cmd/
│   ├── titanus/
│   ├── titanusd/
│   └── titanus-init/
├── internal/
│   ├── ansi/
│   ├── model/
│   ├── planner/
│   ├── preflight/
│   ├── remote/
│   └── setup/
├── runtime/
├── fabric/
├── storage/
├── systemd/
├── scripts/
├── docs/
├── Makefile
└── go.mod
```

## Development rule

> **Capabilities may solve problems also solved by existing platforms, but Titanus implementation and abstractions must remain Titanus-native.**

Accordingly:

- no Docker source is imported;
- no Kubernetes source is imported;
- no containerd or Podman runtime is used as a hidden backend;
- no Kubernetes API clone is required;
- no Kubernetes YAML object model is used internally;
- external systems such as Ceph are integrated through Titanus-owned adapters;
- standard operating-system interfaces and normal open protocols may be used.

## Roadmap

### Phase 1 — Core bootstrap
ANSI/TUI installer, Realm model, node inventory, Plan engine, SSH/pre-flight layer, configuration persistence and daemon lifecycle.

### Phase 2 — Native Unit runtime
Namespaces, cgroups v2, OverlayFS, process supervision, security profiles, local Fabric and local Disks.

### Phase 3 — Realm clustering
Node identity, secure enrollment, state replication, placement, Pulse monitoring, recovery and multi-node Fabric.

### Phase 4 — Distributed storage
Ceph adapter, RBD, CephFS, RGW, storage domains, snapshots and failure-aware reattachment.

### Phase 5 — Application orchestration
Fleets, Routes, health-driven placement, rolling updates, autoscaling, Tasks and Vault.

### Phase 6 — Titanus Command Center
Web UI, topology, metrics, nodes, Fleets, storage, logs/events and guided cluster expansion.

## Project status

```text
Project: Titanus Core
Status:  M1-M7 implemented/native complete; independent lab acceptance pending
Release: v0.4.0-rc.10
Target:  Linux / Debian first
Core:    Go
License: To be decided
```

---

**Titanus — one Realm, one control experience, from a single Unit to a distributed infrastructure platform.**

**Development And Design by antonios.mortos@outlook.com**

## Runtime and API production work

Native lifecycle/recovery now includes restricted PID 1 supervision, orphan
reaping, durable exit status, pidfd identity checks and lease restoration.
See [runtime lifecycle](docs/RUNTIME_LIFECYCLE.md).

The mTLS API uses CA-signed `node`, `controller` and `admin` roles. Agents rotate
certificate/key pairs before expiry and synchronize signed CRLs. Existing
role-less certificates require reissuance; see
[API identity and migration](docs/API_IDENTITY.md). The complete production
milestones and remaining failure testing are tracked in issue #8.

## Replicated Realm control plane

Plans with 3 or 5 CONTROL nodes now use embedded Raft inside `titanusd` to elect
one quorum-backed leader and durably replicate Realm objects, assignment state
and Fleet rollback histories. Followers and minority partitions reject Realm
operations. Agents and Gateways discover the available leader among explicitly
configured mTLS endpoints; the reconciler verifies quorum before node actions.

The guided versioned installer configures all voters and a separate authenticated
consensus port (default 9444). `titanus realm consensus` reports local Raft status. Standalone
single-controller behavior remains available; HA-managed state cannot be edited
through the old direct-file path. Native AMD64/ARM64 CI exercises real TLS quorum,
leader loss, partition/rejoin, full log restart and snapshot recovery.

See [Control-plane HA](docs/CONTROL_PLANE_HA.md) for configuration, migration and
precise consensus limits: 3/5 static voters and a 512 KiB Realm entry cap. Phase-2
[HA lifecycle](docs/HA_LIFECYCLE.md) extends the original baseline with verified
Source distribution, quorum-controlled signing and fenced Gateway VIP ownership.
Release-candidate verification includes isolated-node transport and failure tests; physical host/site acceptance remains separate.

## Mandatory native Unit isolation

Newly started Units use separate mapped UID/GID ranges, an eBPF cgroup v2 device
allowlist, read-only/masked sensitive proc paths and a required Landlock LSM policy.
Missing kernel enforcement aborts startup. Native AMD64/ARM64 CI checks real denied
access, descendant inheritance and restart alongside the existing runtime tests.
See [Unit isolation](docs/ISOLATION.md) for dedicated-node UID reservations,
Landlock ABI >=3, legacy layer migration, writable Disk ownership and LSM scope.
Isolation is integrated with the owned Disk lifecycle and authenticated observations below.

### Disk lifecycle and ownership

The native Disk adapter supports offline local/RBD/CephFS snapshots and restore
to new Disks, exclusive Unit attachments retained by namespace PID 1, and explicit
CephFS storage-level eviction. Storage administration uses CLI and authenticated
admin APIs. Native AMD64/ARM64 Ceph integration is exercised in CI. See
[Storage lifecycle](docs/STORAGE_LIFECYCLE.md) for migration, identity mapping,
exact-instance fencing and opt-in automatic remote storage failover. See
[Storage failover](docs/STORAGE_FAILOVER.md) for catalogs, quarantine, ownership
and the deferred independent-VM acceptance matrix.

## Authenticated native observations

The daemon now exposes real metrics, diagnostics and structured audit events over
its existing authenticated listeners. API mutations require durable audit intent;
new Unit starts use independently surviving, byte-bounded log sinks. See
[Observability](docs/OBSERVABILITY.md) for commands, authorization, redaction,
node-local retention, partial-collection and logging-failure behavior.

## Native orchestration and Command Center

CPU autoscaling consumes actual per-Unit cgroup counters with complete/fresh
measurement gates, bounds, cooldown and persisted downscale windows. Durable
Tasks retain confirmed exits or explicit UNKNOWN outcomes without replaying an
ambiguous dispatch. Version-pinned AES-256-GCM secrets are stored as ciphertext
and decrypted only at native launch with separately provisioned private keys.

The embedded `/command-center` UI requires an admin client certificate and uses
real same-origin APIs for Realm/Fleet/Task state, scaling, rollback, autoscaling,
secret metadata and audited administration. It includes responsive mobile layouts.
See [Orchestration, encrypted secrets and Command Center](docs/ORCHESTRATION.md)
for API/CLI examples, at-most-one dispatch semantics, CPU-only/disk-free scaling,
HA key provisioning, immutable versions and retained-state capacity boundaries.

## Native release candidates

All four binaries report one version/revision/state profile. `make release`
creates verified native Linux AMD64/ARM64 archives with manifests and SHA-256
checksums. The versioned installer activates an immutable binary selection,
retains the prior compatible release and preserves configuration, live Units and
persistent data through install/upgrade/binary rollback checks. A publication
workflow accepts only packages from successful main CI at the exact tested SHA.

CI includes five isolated real daemon/agent endpoints, actual VXLAN Service
traffic, rolling rollback, controller SIGKILL and execution-network loss with
Pulse expiry and lease fencing. These nodes share a runner kernel; physical host
reboot, site networking and hardware acceptance are not implied. See
[Release and recovery](docs/RELEASE.md) for prerequisites, migration, key/state
backups and the distinction between binary rollback and data/schema rollback.
The current candidate line is `0.4.0-rc.10`; published `rc.1` through `rc.10`
remain preserved project checkpoints. Phase 2 is tracked in issue #20. All M1-M7
implemented/native milestones are complete; code/CI validation remains separate
from pending independent-VM and physical-host acceptance.

Phase-2 HA adds quorum-committed PKI policy/signing sequence, verified controller
Source replication/recovery, and fenced Gateway VIP epochs. See
[HA_LIFECYCLE.md](docs/HA_LIFECYCLE.md) for private-key provisioning, optional
out-of-band power fencing, and deferred independent-host acceptance.

The versioned terminal installer and its offline review/apply workflow are documented in [Guided lifecycle installation](docs/GUIDED_INSTALL.md).

Phase-2 M4 includes authenticated offline host backups plus a coordinated
complete host recovery set bound to one committed Realm revision. The rc.6
identity-preserving Ceph profile exports/imports actual RBD/CephFS bytes and
retains startup blockers until all hosts and storage produce verified recovery
proofs. It refuses changed Ceph FSIDs/objects and snapshot histories instead of
omitting them. See [BACKUP_RECOVERY.md](docs/BACKUP_RECOVERY.md) for prerequisites,
operator fencing, encryption/metadata limits and the exact supported scope.
Independent-host disaster acceptance and fresh-backend/snapshot-history recovery
are not claimed by this profile.

Version/schema transitions: [docs/VERSION_TRANSITIONS.md](docs/VERSION_TRANSITIONS.md).

Phase-2 M6 schema-two orchestration, central structured collection/alerts, reference-preserving key rotation and measured memory/PID policies: [PHASE2_ORCHESTRATION.md](docs/PHASE2_ORCHESTRATION.md).

Measured CI and independent VM/lab tools: [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md).
