# TITANUS Core

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

The ANSI interface collects the Realm topology and network/storage requirements, including management IPs, Fabric IPs, control VIP, Ceph public and cluster IPs, Fabric and service CIDRs, node capabilities and SSH/bootstrap access.

The lifecycle is:

```text
DISCOVER
   ↓
ASK
   ↓
PLAN
   ↓
VALIDATE
   ↓
SIMULATE
   ↓
CONFIRM
   ↓
DEPLOY
   ↓
VERIFY
```

Destructive storage operations are never implied by simply entering an IP or selecting a node. They require an explicit reviewed Plan.

## Current repository stage

**Titanus Core is in early development.**

The initial bootstrap focuses on:

1. Native Titanus data model.
2. ANSI/color CLI and setup flow.
3. Realm and node planning.
4. IP/CIDR validation.
5. SSH reachability/pre-flight checks.
6. Plan serialization.
7. Core daemon skeleton.
8. A clean foundation for the runtime, Fabric, storage and Realm engines.

The project should not yet be treated as a production replacement for mature container or orchestration platforms.

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
Status:  Early development
Target:  Linux / Debian first
Core:    Go
License: To be decided
```

---

**Titanus — one Realm, one control experience, from a single Unit to a distributed infrastructure platform.**

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

`realm deploy` configures all voters and a separate authenticated consensus port
(default 9444). `titanus realm consensus` reports local Raft status. Standalone
single-controller behavior remains available; HA-managed state cannot be edited
through the old direct-file path. Native AMD64/ARM64 CI exercises real TLS quorum,
leader loss, partition/rejoin, full log restart and snapshot recovery.

See [Control-plane HA](docs/CONTROL_PLANE_HA.md) for configuration, migration and
precise limits: 3/5 static voters, a 512 KiB Realm entry cap, Sources pre-staged on
all controllers, designated-primary PKI signing and separate storage fencing.
This does not mark the remaining isolation, storage, observability, UI and release
milestones as complete.

## Mandatory native Unit isolation

Newly started Units use separate mapped UID/GID ranges, an eBPF cgroup v2 device
allowlist, read-only/masked sensitive proc paths and a required Landlock LSM policy.
Missing kernel enforcement aborts startup. Native AMD64/ARM64 CI checks real denied
access, descendant inheritance and restart alongside the existing runtime tests.
See [Unit isolation](docs/ISOLATION.md) for dedicated-node UID reservations,
Landlock ABI >=3, legacy layer migration, writable Disk ownership and LSM scope.
Storage lifecycle/fencing, observability, UI and release remain open in issue #8.

### Disk lifecycle and ownership

The native Disk adapter supports offline local/RBD/CephFS snapshots and restore
to new Disks, exclusive Unit attachments retained by namespace PID 1, and explicit
CephFS storage-level eviction. Storage administration uses CLI and authenticated
admin APIs. Native AMD64/ARM64 Ceph integration is exercised in CI. See
[Storage lifecycle](docs/STORAGE_LIFECYCLE.md) for migration, identity mapping,
manual fencing and the boundary against automatic unreachable-writer failover.
