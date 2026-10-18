# Editions, node accounting, and release roadmap

## Community Edition (current build)

The Community Edition supports up to **9 Realm nodes**, regardless of
installation topology:
- Standard Titanus Core
- Titanus Native HA (planned installation profile)
- Advanced / Custom (planned installation profile)

A Realm node is a distinct participating **virtual machine or physical host**,
independent of VM ID, management address, or capabilities. A host with multiple
roles (CONTROL, EXECUTION, STORAGE, GATEWAY) is counted **once**. A node
reinstalled with the same Realm identity is still one node. The nine-node
limit does **not** count Units, Fleets, application instances or containers.
Available RAM, CPU, networking and storage still constrain workload capacity.

The Community installer prompts for 1–9 nodes. The `RealmPlan.Validate`
implementation also rejects any plan with more than 9 entries, including
hand-edited JSON plans or non-wizard deployment paths that use the plan
validator. This is a Community build policy, **not** a license for Enterprise
operation. The implementation currently checks entries in a deployment
plan, not authenticated global usage across unrelated Realm instances.

### Important limitation for existing 10-node labs

Before upgrading an existing deployment containing more than nine nodes,
operators must review the edition transition. Current Community plan validation
will reject a 10-node plan on that build. The installer never deletes VMs,
units, Ceph devices, or Realm data to force a plan under the limit.
Do not remove a live Ceph, quorum or execution node merely to reach the count;
plan a safe topology or edition migration separately.

## Enterprise Edition — Soon Available

**Current status: Soon Available.**

Enterprise is planned to remove the fixed **edition-imposed node ceiling** for
both Standard and Native HA cluster installations. It does not imply infinite
physical capacity or validated scale at arbitrary cluster size.

A **90-day trial** is planned for its public release. An exact release date,
eligibility, activation, licensing and download procedures have **not yet been
published**. Enterprise is not selectable in the current Community binary,
and this Community branch does not implement Enterprise license/trial enforcement.
Do not represent Enterprise as already shipping.

## Installation UX direction

The shared installation flow under development is:

1. Choose the number of Realm nodes, 1–9 in Community.
2. Enter management IPs and an authorized bootstrap SSH account per node,
   using a common initial root password when explicitly chosen by the operator.
3. Verify SSH identities; bootstrap safe passwordless keys between the selected
   nodes; require successful all-pairs verification before proceeding.
4. Choose Standard, Native HA, or Advanced/Custom profile.
5. Assign roles, network, storage, preflight and reviewed deployment plan.

The SSH mesh and profile menu are a **design proposal**, not implemented by
this edition-limit change. Private keys stay private, host fingerprints must
be verified, secrets must not be stored in plans or logs, and existing node
keys/authorized_keys must never be destructively replaced. Implement these
as separately reviewed changes with automated tests and operator opt-in.

## Release gate

Publish Community node-cap releases only after passing model, TUI, plan-file
and deployment validation tests. Enterprise claims additionally require
licensing/trial implementation, scaling tests and public release artifacts.
