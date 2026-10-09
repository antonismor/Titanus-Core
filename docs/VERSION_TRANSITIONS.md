# Protocol and data transitions

Schema zero is the existing rc.5/rc.6 JSON/Raft representation. Binary installation
never migrates data. rc.7 advertises protocol 1 and read/write schemas 0–1 via
`--capabilities-json` (all four binaries) and authenticated `GET /v1/compatibility`.
The release manifest and `titanus-state/v3` profile remain unchanged. An
unadvertised legacy node is admitted only while schema zero is active.

## Controller-first binary upgrade

Use the verified guided versioned installer one controller at a time, keeping a
quorum available. Upgrade all original controllers before upgrading agents,
because old controllers reject the new agent capability field. During this
phase the old/new controller binaries use the unchanged schema-zero state.
Then upgrade each worker/Gateway daemon and agent. Agent re-registration
publishes its capabilities; verify every registered node, including disabled or
quarantined nodes, supports the target. The prepared installer already orders
controllers before other nodes. It does not choose a schema automatically.

Preserve an authenticated, complete pre-migration recovery set and all binaries.
Record its SHA and the original membership. Independent power/writer fencing
and a consistent offline recovery point are still required for data rollback.

## Explicit schema 0 → 1

Run on the quorum leader's protected Unix socket:

```
titanus schema status
titanus schema migrate operator-migration-2026-001 EXPECTED_REVISION
```

Use a unique 16–128 character safe migration ID and the exact inspected Realm
revision. The admin endpoint reads live capabilities directly over mTLS from
**every original voter and registered node**, including inactive nodes; missing,
legacy, incompatible or mismatched host/Realm responses stop preparation.
Every host durably writes an archived `compatibility/schema-floor.json` plus a
matching legacy `.recovery-pending` blocker before the leader proposes one
revision-checked schema transaction. Original binaries refuse that blocker.
Current binaries accept it only when both private floor documents agree and
are compatible. Installation and floor preparation serialize on each host.

Schema one adds an immutable execution identity to each existing Task. Its
phase, Unit/run ID, lease, exit and unknown outcome are preserved. New Tasks
receive an identity at creation. Replication rejects unsupported schema, data
rollback and identity/marker changes. Placement/registration require explicit
schema-one node capabilities. This transition does not retry unknown Tasks.

A partial prepare leaves durable floors; it does not change quorum state. Resume
with the **same migration ID**, after inspecting the current revision. A failed
or uncertain quorum response must first be resolved by `schema status`. Retrying
an already committed ID returns its original receipt without another commit.
Do not delete floors to bypass an interrupted migration. Hosts may already
have newer state; arbitrary deletion would permit destructive binary rollback.

## Binary and data rollback limits

Before preparation, same-profile schema-zero binary rollback remains available.
After any floor is prepared, all four selected binaries must advertise compatible
protocol and schema-one read/write support. The installer verifies each binary
and rejects legacy/incompatible candidates **before stopping services or switching
selectors**, including automatic recovery of a failed installation. It reads the
configured live state root; alternate-root tests inspect that root's standard
`var/lib/titanus`. Arbitrary custom layouts require explicit offline migration.

Schema one cannot be downgraded in place. Data rollback requires restoring the
complete authenticated pre-migration recovery set offline with exclusion of old
actors and storage writers. A backup of migrated state includes the private
floor. Restore atomically replaces its ordinary legacy recovery blocker with
that archived floor at completion, so old binaries cannot start after DR.
Private recovery sets and signing keys must be retained independently.

## Evidence and limits

Unit tests cover legacy admission, preservation of UNKNOWN execution, stale
revision, interrupted quorum commit, idempotence and rollback rejection. Native
AMD64/ARM64 CI builds the actual immutable tested rc.5 source at
`d85d76626f72a9f642363a2eaa34a853a792573d`, rolls three real processes to the current
binary while writing schema-zero state, prepares/commits schema one, kills the
leader, rejects startup of that actual old binary and reconstructs a writable
schema-one quorum. Separate native tests destroy and restore all three migrated
voter roots, verify archived floors and unchanged UNKNOWN execution identity,
and commit new writes. The installer is exercised with actual verified legacy
and current bundles: pre-migration rollback succeeds, post-preparation rollback
and legacy reinstall are refused before selection changes. Local floor checks
are bound to the exact committed Realm and migration. Disposable native CI shares a host kernel/network. Actual
mixed-version independent-host rolling upgrades remain pending on the central
large server. No user server is operated by this change.

## Schema 1 → 2 (rc.8)

The next explicit step enables the M6 policies; use `schema migrate ID REVISION 2`.
Every original host must support schema two before its monotonically higher
floor is prepared. The migration extends the immutable chain without modifying
existing execution identities or operational state. rc.7 startup/reinstall and
rollback are refused after preparation. See PHASE2_ORCHESTRATION.md for interrupted
floor completion, key provisioning and the evidence/independent-host boundaries.
