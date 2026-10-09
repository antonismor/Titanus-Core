# Native observability — milestone 8

The running daemon serves these endpoints over the existing privileged Unix
socket and certificate-authenticated mTLS listener. Admin and Controller roles
can read observations; Node and unauthenticated callers cannot. Existing CRL,
expiry and origin checks apply. No separate anonymous HTTP listener is opened.

| CLI | API | Result |
| --- | --- | --- |
| `titanus metrics` | `GET /v1/metrics` | Prometheus text |
| `titanus diagnostics` | `GET /v1/diagnostics` | actual Realm/local runtime/Disk counts |
| `titanus events` | `GET /v1/events` | last 256 retained structured records |
| `titanus unit logs ID` | `GET /v1/node/units/ID/logs` | bounded raw workload tail; API requires admin |

Metrics include HTTP outcomes/duration sums, state inventories, application
readiness/liveness, restart counts, log sink degradation and observation write
failures. Request counters reset on daemon restart; runtime counters/inventories
come from persisted state. Labels use finite route/method categories, not Unit
IDs, query strings or user data. No sample is emitted for an absent state; its
count is zero. A failed collector is explicitly incomplete: diagnostics returns
HTTP 503, and the metrics endpoint reports `titanus_diagnostics_complete 0`.
Metrics are operational evidence, not a claim that ACTIVE means application-ready.

The cluster section describes the local controller's replicated Realm view.
Runtime, Disk and logging inventories describe this daemon's node. Scrape every
node separately. There is no hidden remote fan-out or invented cluster-wide
application/log inventory. Followers expose their local view, not leader-authorized
writes. Node state freshness remains governed by recorded pulse/HA behavior.

## Durable API audit and structured events

`STATE_ROOT/observability/events.ndjson` stores an fsynced intent **before** any
non-read API handler runs, and an outcome afterward, with a generated request ID,
CA-verified actor/role, method, route category, target-path SHA-256 and status.
`X-Titanus-Request-ID` correlates responses. No body, query string, authorization
header, certificate, environment, command or error message is accepted into this
journal. Structured Unit status, readiness/liveness and restart transitions are
also recorded by the daemon runtime. Detailed workload output is not attached.

If an intent cannot be durably written, the operation is rejected with 503 and
the handler never executes. An outcome failure after execution cannot undo that
operation: the retained intent can be unresolved, and observation failure metrics
increase. A crash between intent and outcome is likewise unresolved, not a false
success. Recovery reconciles actual persisted state; callers must inspect before
retrying potentially non-idempotent actions. Background state-event failure does
not prevent emergency fencing/stopping; failure counts expose that degradation.

The journal keeps current and previous segments, each at most 1 MiB, and readers
return at most 256 events. It is node-local, bounded retention, not replicated,
immutable or a compliance audit service. Privileged administrators can change
files or run out-of-band local CLI/kernel operations; those are not API audit
transactions. Export/scrape records before retention expires if long-term evidence
is required. Existing daemon service logs remain under the service manager's own
retention policy; they are not included in observation responses.

## Bounded independent workload logging

New Unit starts launch a native host log sink through `titanus-init`. Namespace
PID 1 and the host monitor receive only an output pipe, never host log file
descriptors or directories. The sink survives daemon/monitor SIGKILL while PID 1
still holds that pipe, rotates `logs/unit.log` and `.1` at 1 MiB each, then exits
when all writers close. It opens files without following symlinks and rejects
non-regular files. Raw data can span segment boundaries. No unbounded line buffer
or per-line parser is used, so even one enormous binary line remains bounded.
Pipe backpressure is possible during ordinary disk I/O, as with other pipe loggers.

On disk-write failure the sink records a best-effort, preallocated `sink-status`
marker and drains/discards subsequent output instead of killing or indefinitely
blocking the workload. Diagnostics and metrics expose that marker. The startup
handshake fails closed if the initial sink cannot open its bounded files. Storage
errors can prevent the marker itself being persisted; this is not a lossless log
delivery guarantee. Workload segments are not fsynced per chunk; audit is.

Workload logs are **raw application data** and may contain application secrets.
Only administrators can retrieve them through the API. Redaction-by-construction
applies to structured audit/events/diagnostics, not arbitrary application output.
Do not log secrets from applications. Already-running legacy Units retain their
old output file descriptor until stop/restart; on restart oversized legacy files
are reduced to their last 1 MiB. CLI/API reads are bounded even before restart.

## Verification

Tests exercise concurrent byte-bound rotation, legacy tail preservation, unsafe
file/symlink rejection, finite metric categories, payload/header/query exclusion,
audit intent/outcome correlation, fail-closed mutation logging and incomplete
diagnostics. Native AMD64/ARM64 CI kills a real Unit's monitor, floods its retained
logger with 3 MiB of binary output, checks both file bounds and preserved Disk
ownership, and verifies drain-on-storage-error. Real daemon restart tests inspect
diagnostics/metrics/events and an audited stop through its Unix API. Existing
isolation, HA, health, rollout and actual Ceph tests remain enabled.

This milestone does not finish Tasks, autoscaling, encrypted secrets, Command
Center, versioned release or multi-node release certification (milestones 9–10).

Phase-2 M6 adds a separate bounded controller collection of structured audit/runtime
logs and coverage alerts. The node-local/raw-output limits above still apply; see
[PHASE2_ORCHESTRATION.md](PHASE2_ORCHESTRATION.md).
