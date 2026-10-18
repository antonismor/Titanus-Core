# Editions, node accounting, and release roadmap

## Community Edition (current build)

The Community Edition supports up to **9 Realm nodes**, regardless of
installation topology:
- Standard Titanus Core
- Titanus Native HA (opt-in fresh-install profile, 5–9 hosts)
- Advanced / Custom

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

The Universal Installer candidate implements the profile menu and directed SSH
trust bootstrap in [Phase A](universal-installer-phase-a.md), and the fresh Native
HA profile in [Phase B](universal-installer-phase-b.md). Native HA requires three
or five separate CONTROL voters and at least two EXECUTION Workers, all within
the Community nine-host ceiling. [Phase C](universal-installer-phase-c.md) adds
Ceph HA and an explicit backup-bound transition for existing 3/5-voter Realms
with 5–9 hosts. Single-controller membership conversion remains blocked.
Private keys stay private, host fingerprints require approval, secrets are not
stored in plans/logs, and existing node keys/authorized_keys are preserved.
Fenced Workers require explicit, verified capacity recovery; no automatic
power-on or application relocation is enabled.

## Release gate

Publish Community node-cap releases only after passing model, TUI, plan-file
and deployment validation tests. Enterprise claims additionally require
licensing/trial implementation, scaling tests and public release artifacts.
