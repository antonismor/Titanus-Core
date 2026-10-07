# Rolling Fleet updates and rollback

Submit a complete Fleet JSON with `titanus fleet apply fleet.json` (POST
`/v1/realm/fleets`). This publishes a new generation and retains existing
assignments. Fleet mutations require administrator authorization. Creation
does not require immediate capacity: desired state is accepted and placement
advances when eligible nodes become available.

`minimum_available` is a readiness budget. A Unit counts only after the node
reports ACTIVE **and ready**, the node is READY and its execution lease is
valid. The controller refreshes runtime readiness before choosing retirements.
It never intentionally retires a ready Unit below that budget. Independent
application failures or simultaneous node failures can still reduce actual
availability; the budget is not a guarantee against those failures.

`max_surge` defaults to 1 (range 1..100). Total tracked assignments cannot
exceed desired replicas plus surge. New capacity excludes unacknowledged
retirements. Stop, revoke and delete failures retain the assignment. A
successfully stopped Unit awaiting deletion is recorded as STOPPED and is not
restarted. A controller crash can resume these operations idempotently.
Published host ports and node resource capacity can stall surge placement.

Old Units always use their pinned historical template, security, health,
environment and placement requirements. The last ten revisions are retained,
plus any older revision still referenced by a Unit. Histories supplied by
clients are ignored. Realm replacements use file and directory fsync.

Read rollout progress and blockage reason with `titanus fleet status NAME`.
An unready replacement fills a surge slot and leaves healthy old Units running.
There is no automatic rollback timeout: an administrator chooses a revision:

```sh
titanus fleet rollback web        # immediately previous desired revision
titanus fleet rollback web 1      # a specific retained generation
```

POST `/v1/realm/fleets/web/rollback` takes
`{"generation":1}`; generation zero chooses the previous revision. Rollback
publishes the chosen full Fleet spec as a **new** generation, including replica
count and availability settings. Scaling alone retains the generation.

Shared writable Disks block concurrent replacements, including if the old Unit
is on another node. A replacement can start only after acknowledged retirement
removes the old writer. With one stateful replica and minimum availability one,
a safe stop/start rollout cannot meet that budget; lower the budget or design
independent storage. Unreachable writable writers are never released solely
because the scheduling lease expires. Storage fencing is a separate milestone.
Deleted Fleets on unreachable nodes retain pending cleanup.

CI tests exercise good updates, failed readiness, minimum availability and surge,
failed stop/delete, persisted rollback, history validation and writable-disk
blocking. Privileged Linux AMD64 and ARM64 jobs additionally run real
namespace/cgroup/seccomp Units through update, unhealthy revision and rollback.
That test uses a local runtime adapter; multi-node transport/failure testing is
tracked separately in the release milestone.
