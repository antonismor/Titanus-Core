# Orchestration and Command Center

Titanus extends its native Unit runtime and replicated Realm state. No Docker,
Kubernetes, containerd or external metrics server is required for this profile.

## CPU autoscaling

An admin installs a policy using `titanus autoscale apply policy.json` or POST
`/v1/realm/autoscalers`:

```json
{"fleet":"web","min":2,"max":10,"target_cpu":60,"cooldown_seconds":60,"downscale_seconds":300}
```

The controller reads each active Unit's actual `cpu.stat` usage counter through
GET `/v1/node/units/ID/usage`. The response also reports `memory.current`, the
run identity and collection time. CPU utilisation is the difference in CPU
microseconds over the difference in wall time, divided by the Unit's explicit
CPU quota. Allocated host CPU or memory is never substituted for usage. The
recommendation is `ceil(instances * mean quota utilisation / target)` with 10%
tolerance, policy bounds, cooldown and a continuous low-load window before
shrinking. Policy timing is replicated; counter baselines are process-local.

All assignments must be from the current generation, ready, on ready Nodes and
under a live lease. Missing, stale, decreasing or different-run counters hold
scaling and clear the low-load window. A new leader collects fresh baselines.
The first sample cannot change the instance count. Scaling changes desired
instances without replacing the template generation; normal readiness budgets
and acknowledged retirement still apply. Manual scaling respects policy bounds;
disable the policy before scaling outside them. Incompatible template/availability
changes require updating or disabling the policy. Initial instances outside the
policy's bounds are corrected only after complete valid measurements.

This first profile supports CPU-driven scaling for **disk-free Fleets with an
explicit CPU limit**, at least one instance, max 1000, target 10–90%, cooldown
10–3600 seconds and low-load windows 30–86400 seconds. Min cannot be below
minimum_available. It does not scale host Nodes, scale to zero, claim memory-based
scaling, or attach a single writable Disk to additional instances. Sources still
need staging on every controller as described in CONTROL_PLANE_HA.md.

## Durable Tasks

`titanus task submit task.json` creates an immutable named execution:

```json
{"name":"job-20261008","template":{"source":"app","command":["/bin/sh","-ec","exit 7"],"memory_bytes":67108864,"cpu_percent":50,"pids_max":64}}
```

Tasks use the existing placement engine, Source transfer, leases, security
profiles, Disk ownership and native PID 1. The runtime restart policy must be
`never`. Each Task owns a deterministic `task-NAME` Unit and records its assigned
Node, dispatch state, run identity and confirmed exit code in Realm consensus.
The API overwrites client-supplied status fields. A duplicate name is rejected;
Task execution records are retained (up to 128 per Realm in this profile).

A Task progresses through PENDING, PREPARED, DISPATCHED and RUNNING. Dispatch is
committed **before** invoking Start. A lost response, unreachable Node, missing
exit record or crash between committed dispatch and request delivery yields
UNKNOWN. The controller can recover actual active/terminal state by inspecting
the original Node and run. It never retries an uncertain Start or moves that Task
to another Node. This deliberately provides at-most-one controller dispatch,
not exactly-once execution of application side effects. Operators with root/admin
access can override runtime state; that is outside the scheduling guarantee.

Only a confirmed matching exit record produces SUCCEEDED (code 0) or FAILED
(nonzero). Setup errors without a workload exit remain UNKNOWN. Confirmed terminal
Tasks never restart after controller/daemon/leader recovery. Task state retains
history even when the node-local logs have rotated.

`titanus task cancel NAME` / POST `/v1/realm/tasks/NAME/cancel` records intent.
For dispatched Tasks, CANCELLED requires the assigned Node to acknowledge Stop
and lease revocation. An unreachable writable execution must be fenced before
any new Task using the same storage or external side effects. UNKNOWN does not
prove the application stopped. Task cancellation before assignment is safe;
PREPARED Tasks have not had a controller Start dispatch. There is no automatic
retry, cron schedule, parallel job set or Task migration in this profile.

## Encrypted secrets

Generate a keyring with `titanus secret keygen /etc/titanus/secrets.json` (creates
exclusively, mode 0600, fsyncs file/directory). Provision that private file
separately and set `TITANUS_SECRET_KEYRING=/etc/titanus/secrets.json` on authorised
controllers and execution Nodes. Do not commit the file or put it inside a
Source. Key material is never generated implicitly by the daemon, replicated in
Realm state, exposed by an API or sent in reconciliation requests.

```sh
titanus secret put database-password < /path/to/private-value
# Create a new immutable version on every successful put; list metadata only.
titanus secret list
```

PUT `/v1/realm/secrets/NAME` takes `{"value":"BASE64_BYTES"}` over existing
admin mTLS or the privileged Unix socket. Values are 1–16384 bytes, without NUL;
max 32 versions per name, max 32 references per workload. AES-256-GCM uses a fresh
random nonce and authenticates the profile, Realm, name, version and key ID.
Realm JSON, Raft log/snapshots, rollback histories and Unit specifications retain
ciphertext and pinned references, not decrypted payloads. Admission and durable
audit happen before the encrypted record is committed. The metadata API never
returns plaintext. Generic Realm state contains encrypted records only for
admin/controller; Node state removes the vault, environments and Task leases.

References use explicit versions and distinct environment names:

```json
{"secrets":[{"name":"database-password","version":1,"environment":"DB_PASSWORD"}]}
```

The controller attaches only referenced ciphertext to that Unit's spec. The
execution Node authenticates its configured Realm and each binding, loads its
private keyring and decrypts immediately before native launch. Missing keys,
wrong Realm, changed identities or ciphertext prevent exec. Plaintext travels in
live process environments; it is never written to a runtime spec or passed in
command arguments. Workloads inherit a known PATH and their declared environment,
not the daemon's environment. This does not hide values from host root, the
workload itself, its child processes, or an application that prints them; raw
workload logs remain admin-only. Existing plaintext environment fields are still
user-controlled and are not automatically converted into secrets.

The private JSON keyring format is `{"active":"key1","keys":{"key1":"BASE64_32_BYTES"}}`.
For rotation, provision an additional 32-byte key on **every** authorised
controller and execution Node, keep old keys, then change `active` and put new
versions. Update Fleet/Task references explicitly. Do not remove an old key while
any retained spec, rollback history, live workload or restore snapshot needs it.
The daemon reloads keys at each put/start; existing processes retain their live
values. Referenced secret deletion is rejected, including rollback and retained
Tasks. Deleting an unreferenced name clears ciphertext while retaining version
tombstones, so recreation cannot reuse an old version. Node-local orphaned specs
and existing environments require explicit operator stop/removal; deleting Realm
metadata does not revoke live process memory.

**HA boundary:** encrypted records and references replicate; keyrings do not.
All controllers and execution Nodes holding a shared Realm key are trusted to
decrypt material under that key. This is not per-Node cryptographic compartmentalisation,
a KMS, or automatic key distribution/rotation. Back up keyrings securely and
separately; a restored state without its keys cannot execute secret workloads.
The existing 512 KiB HA state-entry limit still applies and may reject oversized
collections before their count limits. Task archiving and vault compaction are
not implemented; capacity errors are explicit rather than discarding history.

## Authenticated Command Center

Open `https://CONTROLLER:9443/command-center` with an existing CA-signed **admin**
client certificate (and its private key) installed in the browser. Keep the HTTPS
endpoint, CA trust, certificate renewal/revocation and configured controller
origins as described in API_IDENTITY.md / CONTROL_PLANE_HA.md. No anonymous web
listener, browser token or shared password is introduced.

Embedded assets show current Nodes, assignments, Fleets, Tasks, encrypted secret
metadata and the latest node-local audit events. Controls scale/roll back Fleets,
configure/delete autoscalers, submit/cancel Tasks and create secret versions using
real same-origin APIs. Secrets are never returned to the page and the password
input clears after submission. Browser errors show unavailable/stale state;
the UI contains no invented metric data. A follower rejects Realm operations
using the same leadership gate as the CLI; open the current configured leader
when it reports unavailable. There is no transparent replay of ambiguous writes.

All assets require admin authentication, no-store responses and a restrictive
CSP (self scripts/styles/API only; no framing, forms or base URLs). Dynamic
values use textContent, not HTML interpolation. HTTPS Origin checks, signed
roles and CRL validation still apply per request. The layout adapts to mobile.
It uses no external CDN, analytics or dependencies. Direct APIs/CLI remain the
administrative interface for Source/Disk lifecycle, Fleet creation and PKI.

## Verification

Local tests cover authenticated encryption/tampering, immutable versions,
durable Task outcomes, lost Start responses, crash-before-delivery ambiguity,
acknowledged cancellation, actual CPU delta recommendations, missing/restarted
counter rejection, cooldown/downscale persistence, role denial and fail-closed
audit admission. Actual three-voter TLS/Raft tests cover replicated encrypted
records/Tasks/policies, follower write rejection, leader loss and FSM snapshot
recovery without plaintext. Native AMD64/ARM64 CI additionally executes a Task
with an injected secret and exit 7, denies a missing-key launch, checks persisted
state/specs for plaintext, and scales a real CPU-burning Unit from cgroup deltas.
Existing runtime, isolation, HA, health, rollback and native Ceph CI remain enabled.
