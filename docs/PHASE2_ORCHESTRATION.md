# Phase 2 observability and orchestration (M6)

rc.8 keeps the v3 state profile. Install binaries controller first while the old
schema remains active. After every original voter and registered daemon/agent is
upgraded, explicitly migrate 0→1 if necessary, then 1→2 with a different ID:

```sh
titanus schema status
titanus schema migrate m6-schema-two-20261009 EXPECTED_REVISION 2
```

The same all-host live capability/floor preparation applies. Schema-two data,
Task schedules, secret rotation receipts and additional metric policies are
refused by rc.7/older binaries, including installer rollback. Floor records
advance monotonically. Historical schema-one Raft entries may replay under a
higher prepared floor; the complete committed migration chain remains immutable.
An interrupted two-file floor publication blocks startup. With all local actors
stopped, use `titanus schema prepare-floor PRIVATE_JSON` to resume the **same**
inspected Realm/migration/schema floor, never to lower/remove it. This command
requires exclusive offline maintenance and refuses an existing restore blocker.
Backups preserve the newest archived floor. The M4 identity-preserving Ceph,
metadata, fencing-attestation and unencrypted private-backup limits still apply.

## Central structured logs/events and alerts

Controllers poll `/v1/node/observation` over existing mTLS. The quorum leader
collects original controllers and registered nodes every 15 seconds, with a
15-second iteration budget, at most 64 hosts, 256 events per host and finite
HTTP client deadlines. Missing, stale, wrong-identity/Realm or over-capacity
hosts reduce coverage; collection never invents observations for them.

`GET /v1/realm/observations` (or `titanus schedule events`) exposes the latest
256 centralized structured audit/runtime events. The journal retains at most
two 1-MiB segments. Only fixed event kinds, timestamps, statuses, booleans and
bounded restart counts are retained; actor, object, request and node identities
are SHA256 hashes. Free text, command/environment/path/error fields, secrets and
raw application/service logs are excluded. Existing admin-only local workload
logs remain bounded. Arbitrary application output cannot be reliably redacted;
this collector deliberately accepts the structured log profile only.

`GET /v1/realm/alerts` / `titanus schedule alerts` returns machine-readable
coverage, collection age and fixed alerts for unreachable/stale hosts, retention
gaps, incomplete diagnostics, degraded audit/logging and UNKNOWN Tasks. An
absent/incomplete/stale collection returns 503 with the evidence body. Scrape
these endpoints or use an external alert manager for notifications; this profile
does not send messages. Collection status/cursors persist on the collecting
controller. After leader change, that controller collects its own evidence.
Observation files are bounded node-local data, not a replicated lossless journal.
Partial append/status failures can produce duplicates; source retention can
produce reported gaps. Archive evidence before retention expires.

## Protected key provisioning and reference-preserving rotation

Generate and distribute a new distinct 32-byte raw key through a private channel;
never put it in a Source, command argument or repository. On **every** original
controller and registered execution/Gateway node, including disabled nodes:

```sh
titanus secret key-add /etc/titanus/secrets.json key2 < PRIVATE_RAW_32_BYTE_KEY
```

The protected keyring update is atomic, serialized and rejects replacing/removing
an existing key. Retain all old keys for saved Unit specs, rollback histories,
processes and independent backups. Activate on the quorum leader only after
provisioning every host, then submit the exact inspected revision:

```sh
titanus secret key-activate /etc/titanus/secrets.json key2
titanus schema status
titanus secret rotate rotation-20261009-001 key2 EXPECTED_REVISION
```

The authenticated API checks each original voter/registered node's exact Realm,
identity and fingerprints for **all referenced source keys and the target**.
Missing/mismatched/unreachable hosts prevent any vault transaction. No API
transports key material or plaintext. One revision-checked quorum transaction
re-encrypts every retained non-tombstone version with fresh AES-GCM nonces,
keeping its Realm/name/version and all workload references unchanged. Every
ciphertext is authenticated before commit; any failure aborts the entire update.
An immutable rotation receipt (up to 16) makes a lost-response retry idempotent.

Workers reload keys at launch; new specs receive the re-encrypted bindings.
Existing Unit specs/process environments retain their original values/ciphertext.
Old-key retirement and process-memory revocation are outside this profile and
must not be inferred from successful rotation. Key distribution remains explicit
operator provisioning; readiness assumes trusted administrators do not replace
private keys concurrently out of band.

## Measured memory and PID autoscaling

The existing CPU policy additionally accepts `target_memory` and `target_pids`
(percentages, 10–90; 0 disables a metric). At least one metric is required. Memory
uses actual `memory.current` divided by the declared Memory limit; PID pressure
uses actual `pids.current` divided by PidsMax. CPU uses actual counter deltas.
The highest enabled metric's bounded recommendation wins; downscale requires
all recommendations to be low. Explicit limits, disk-free Fleets, complete ready
current-generation assignments, live leases, matching run identities, fresh
samples, initial baselines, cooldown and persisted low-load windows still apply.
Missing coverage and leader/process changes hold scaling. Memory/PID pressure is
an observed scaling policy, not a guarantee that replication reduces per-instance
memory or application concurrency. Hosts are not autoscaled.

## Scheduled execution and bounded confirmed retries

```json
{"name":"nightly","due_at":"2026-10-10T00:00:00Z","every_seconds":3600,
 "max_runs":8,"max_attempts":2,"retry_seconds":30,
 "template":{"source":"app","command":["/bin/sh","-ec","exit 7"],
 "memory_bytes":67108864,"cpu_percent":50,"pids_max":64}}
```

`titanus schedule submit policy.json`, `list`, `status NAME`, `pause NAME` use the
protected Task-schedule API. Policies and normalized templates are immutable;
pause stops future scheduling, not an already dispatched Task. A one-off policy
uses `every_seconds:0,max_runs:1`. Recurrence is completion-relative UTC wall time,
no parallel overlap or backfill. Clock jumps can shift due time; independent clock
skew testing remains pending. Bounds: 32 schedules, 1–16 runs, 1–4 attempts/run,
retry 1–3600 seconds, repeat 10–86400 seconds, 128 retained Tasks total.

Each due attempt is a distinct Task with immutable execution identity, name,
Unit/run identity and lease. Schedule pointer and Task publish in the same quorum
transaction. The existing dispatch-before-Start/UNKNOWN semantics remain. A
retry or next recurrence requires the original node to return the matching
confirmed exit, acknowledge Stop with PID zero/same run and acknowledge lease
revocation before a new attempt is committed. Controller loss before/after those
steps cannot reset or replay the original Task. UNKNOWN, missing exit, changed
run, unavailable original node or uncertain fencing hold the schedule. Capacity
errors preserve history. These guarantees concern controller dispatch; application
side effects may be repeated after a confirmed failed exit, so design jobs to be
idempotent when opting into retries. No cross-node replay of unknown side effects
or automatic out-of-band power exclusion is claimed.

## Evidence boundary

Tests cover missing-key aborts, readiness mismatch, idempotent receipts, UNKNOWN
hold, original exit/Stop/lease gates, retained identities, fresh/complete metrics,
redaction, persistent collection cursors, coverage alerts and bounded rotation.
Native AMD64/ARM64 CI exercises actual scheduled exits/retries, controller
reconstruction, re-encrypted launch bindings, measured CPU/memory/PID scaling,
schema-two quorum migration, rotation receipt recovery after SIGKILL, original
host collection loss, whole-quorum restart and actual rc.7 startup/installer
rollback refusal. Native CI shares one host kernel; sustained independent-host
partitions, clock skew and workload side effects remain final lab acceptance.
