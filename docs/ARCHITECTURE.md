# Titanus Core Architecture

## Non-cloning rule

Titanus is an independent implementation.

The project may use Linux kernel primitives, documented operating-system interfaces, standard protocols and public interfaces exposed by external infrastructure such as Ceph. It does not embed or disguise Docker, containerd, Podman or Kubernetes as its runtime/control plane.

The internal vocabulary and control model are Titanus-native:

- Realm — complete managed environment
- Node — participating Linux host
- Unit — isolated executable workload
- Fleet — managed group of Units
- Source — Unit filesystem/application blueprint
- Fabric — Realm networking
- Route — traffic exposure
- Disk — persistent storage object
- Vault — secret material
- Pulse — health/state signal
- Task — finite or scheduled work
- Plan — validated infrastructure change description

## Bootstrap control flow

```text
titanus
   |
   +-- ANSI interactive menu
   |
   +-- setup wizard
          |
          +-- Realm identity
          +-- node management IPs
          +-- Fabric IPs
          +-- node capabilities
          +-- SSH/bootstrap identity
          +-- Ceph public IPs
          +-- Ceph cluster IPs
          +-- Fabric/service CIDRs
          +-- Ceph protection/settings
          |
          v
       RealmPlan
          |
          +-- Normalize
          +-- Validate
          +-- Duplicate-IP detection
          +-- Capability validation
          +-- Ceph topology validation
          |
          v
        Planner
          |
          v
      Pre-flight
          |
          +-- SSH
          +-- Linux
          +-- cgroups v2
          +-- OverlayFS
          +-- iproute2
          +-- nftables presence
          +-- privilege test
          |
          v
       Bootstrap
```

The bootstrap engine is intentionally idempotent and non-destructive in the first development stage. Storage-device claiming, filesystem creation, Ceph provisioning and network mutation will be separate explicit Plan stages.

## Runtime target

The Unit runtime will be implemented directly against Linux facilities:

```text
Unit request
    |
    v
Source preparation
    |
    v
Overlay filesystem
    |
    v
cgroups v2 envelope
    |
    v
Linux namespace set
    |
    v
Fabric attachment
    |
    v
Disk attachment
    |
    v
Security profile
    |
    v
titanus-init
    |
    v
Application process
```

## Realm target

A multi-node Realm is designed around capability-driven Nodes instead of rigid node classes.

```text
CONTROL
EXECUTION
STORAGE
GATEWAY
GPU
BACKUP
```

A Node can expose multiple capabilities. The future Placement Engine will score available Nodes using compute headroom, memory pressure, storage locality, Fabric condition, failure-domain diversity, workload policy and reliability.

## Ceph boundary

Ceph is an external storage platform. Titanus will own the integration layer and lifecycle decisions around Titanus Disks.

```text
Titanus Disk Manager
        |
        v
Storage Provider Interface
        |
        v
Titanus Ceph Adapter
      /   |   \
     /    |    \
   RBD  CephFS  RGW
```

Ceph implementation code is not copied into Titanus.

## Safety model

Every infrastructure mutation should be attributable to a Plan. The intended lifecycle is:

```text
DISCOVER -> ASK -> PLAN -> VALIDATE -> SIMULATE -> CONFIRM -> DEPLOY -> VERIFY
```

Destructive storage actions require explicit confirmation and must not be inferred from simple setup input.

## Replicated Realm state

Multi-controller plans with 3 or 5 CONTROL nodes embed the Go Raft library and a
fsync-enabled BoltDB adapter in `titanusd`. Titanus supplies the Realm FSM,
revision validation, static membership, mTLS peer binding and guarded reconciler.
These are explicitly declared libraries; no external orchestrator is used.
Only quorum-committed state is exposed as the live HA Realm, with leader barriers
for API access. See [Control-plane HA](CONTROL_PLANE_HA.md) for its operational
bounds and the state/assets that are outside this replicated FSM.
