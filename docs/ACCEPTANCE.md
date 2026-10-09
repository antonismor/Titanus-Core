# Bounded acceptance tools and independent laboratory runbook (M7)

These tools record a measured scope; they never mark issue #20's independent-host
matrix complete. `summary.json` always keeps `independent_host_acceptance:false`.
One passed workload/fault cannot establish all storage, power, clock, upgrade and
disaster-recovery properties. No tool in this milestone provisions hosts.

## Native disposable CI profile

The five-job CI retains its existing native AMD64/ARM64 runtime and Ceph checks.
Each native runtime job additionally runs:

```sh
sudo env "PATH=$PATH" python3 scripts/acceptance.py ci \
  --seconds 30 --cycles 10 --output /tmp/titanus-acceptance
```

Prerequisites: clean exact committed checkout, Go 1.23.12, all four native
`make build` binaries carrying that SHA, root on a disposable supported Linux
runner, cgroups v2, existing security/kernel prerequisites and the same prepared
BusyBox `/tmp/titanus-rootfs` fixture used by CI. This command must **not** run on
a installed production node. It creates temporary test identities/state, actual
TLS Raft listeners and actual native Units/cgroups. It does not address an
existing cluster. The current test profile is finite: 15–120 requested load
seconds, <=10 synchronous acknowledged writes/s, and 3–32 lifecycle cycles.

The Raft workload records acknowledged revision advancement and p50/p95/max
write timing, isolates the original leader's transport, refuses a minority
write, continues with two voters, rejoins the original logs, restarts all voters
and compares the acknowledged revision/Fleet value on all three. Election and
restart time are measured separately; actual wall time includes them. The native
benchmark records start timings and distinct run identities; each workload must
emit its exact canary once, supply actual cgroup memory/PID usage, acknowledge
Stop for the same run with PID zero, and delete its Unit/cgroup. These checks
fail the job instead of merely printing timings. They are bounded regression
measurements, not saturation or production performance claims.

`summary.json` contains build/revision/architecture, topology, kernel, CPU count,
requested/actual duration, test results, measurements and the raw evidence hash.
`go-test.ndjson` preserves Go's machine-readable events and failure output;
required tests must pass rather than skip. CI uploads `acceptance-ubuntu-24.04`
and `acceptance-ubuntu-24.04-arm`. On ordinary failure the Go fixtures clean up
their own objects. A process timeout/SIGKILL can prevent fixture cleanup: the
summary stays failed with cleanup unverified; retain evidence and destroy the
disposable runner/VM. Do not run broad host cleanup commands.

## Prepare the independent large-server lab

Perform these steps yourself on the central server, after selecting the verified
candidate and reviewing plans. Keep this lab separate from your current services.

1. Create **three controller VMs**, **two worker/Gateway VMs**, an **external
   client VM**, and, for storage scenarios, **three separate Ceph VMs** with
   independent monitor/OSD/MDS processes. Use independently booted supported
   Linux kernels rather than network namespaces. AMD64 VMs on one hypervisor
   establish AMD64 evidence only; native ARM64 CI is separate evidence. Record
   the shared hypervisor/power/storage failure domain explicitly.
2. Allocate explicit vCPU/RAM/virtual-disk budgets. A starting test allocation is
   2 vCPU/4 GiB per controller, 4 vCPU/8 GiB per worker, 2 vCPU/8 GiB per Ceph VM,
   and 2 vCPU/2 GiB for the client. Increase according to workload measurements.
   Give Ceph dedicated **lab-only virtual OSD disks**, separate from boot disks.
   Do not pass through an existing host data disk. Record CPU model, allocations,
   hypervisor version, backing storage and observed limits with the evidence.
3. Provision management, execution underlay and storage networks and an external
   routed VIP observation path. Choose non-overlapping Fabric/Service CIDRs.
   Synchronize clocks and record synchronization status. Verify cgroups v2,
   OverlayFS, mapped UID/GID reservations, Landlock ABI >=3, nftables/iproute2,
   nsenter, Python 3/systemd and, on storage participants, Ceph clients/credentials.
4. Download **both archives and both checksum files** of the candidate. Pin its
   exact main SHA from the issue checkpoint; preserve all prior releases. From a
   checkout of that same tag, verify without executing foreign binaries:

   ```sh
   python3 scripts/verify-release.py --revision EXACT_MAIN_SHA --version 0.4.0-rc.9 \
     titanus-0.4.0-rc.9-linux-amd64.tar.gz titanus-0.4.0-rc.9-linux-arm64.tar.gz
   ```

   The verifier checks outer hashes, all 11 inventory files, manifest identity,
   all four ELF architectures and compiled revision bytes, without extraction.
   SHA files provide integrity; they are not an external signing/KMS identity.
5. Use `titanus setup` / `titanus deploy release` to prepare a private versioned
   installation plan. Review all VM identities, addresses, ports, roles, archive
   hashes and disks before the **operator's explicit apply**. Follow
   [GUIDED_INSTALL.md](GUIDED_INSTALL.md), [RELEASE.md](RELEASE.md) and subsystem
   prerequisites. Start on fresh lab VMs. Preserve operator credentials privately.
6. Upgrade controllers before worker/Gateway daemon/agent binaries; inspect live
   capabilities. Preserve a frozen complete recovery point, then explicitly
   migrate schema 0→1 and 1→2 as [VERSION_TRANSITIONS.md](VERSION_TRANSITIONS.md)
   and [PHASE2_ORCHESTRATION.md](PHASE2_ORCHESTRATION.md) specify. Test rollback
   **before** floor preparation; after migration, prove old startup/install
   refusal. Never remove a floor to manufacture a rollback success.
7. Prepare a deterministic external HTTP canary served through the actual Fleet,
   route/VIP and storage paths under test. It must return a fixed bounded body;
   record the SHA256 of that exact body independently. A default nginx success
   page alone cannot prove the intended application/storage path. Keep external
   client state/backups outside the failed controller/worker VM disks.

## Independent VM observation/load profile

On **each API host VM**, collect facts from its actual installed binary:

```sh
python3 scripts/acceptance.py host-facts --node-id controller1 \
  --binary /usr/local/lib/titanus/current/bin/titanus --output controller1-facts.json
```

Copy these private files to the client with your normal trusted transport. Their
boot-ID hashes must all differ; the runner additionally cross-checks node/Realm,
exact binary revision and architecture live over authenticated HTTPS. Files are
operator-collected hardware/topology evidence, not remote attestation. Retain
independent hypervisor allocations and clock/boot records alongside them.

Create a mode-0600 inventory on the client (replace every sample value):

```json
{
  "format":"titanus-acceptance-inventory/v1",
  "revision":"EXACT_40_CHARACTER_VERIFIED_MAIN_SHA",
  "realm":"TITANUS-LAB",
  "ca":"private/ca.crt","certificate":"private/admin.crt","key":"private/admin.key",
  "nodes":[
    {"id":"controller1","role":"controller","api":"https://10.180.0.11:9443","facts":"controller1-facts.json"},
    {"id":"controller2","role":"controller","api":"https://10.180.0.12:9443","facts":"controller2-facts.json"},
    {"id":"controller3","role":"controller","api":"https://10.180.0.13:9443","facts":"controller3-facts.json"},
    {"id":"worker1","role":"worker","api":"https://10.180.0.21:9443","facts":"worker1-facts.json"},
    {"id":"worker2","role":"worker","api":"https://10.180.0.22:9443","facts":"worker2-facts.json"}
  ],
  "canary":{"url":"http://LAB_VIP:8080/integrity","sha256":"EXACT_RESPONSE_SHA256"}
}
```

All original controllers and registered execution/Gateway hosts must be listed.
The profile admits exactly three controllers, at least two workers and 5–8 API
hosts; Ceph-only hosts are documented separately, not invented API observations.
Relative private paths resolve against the inventory's directory. CA verification
and exact node/Realm/build checks are mandatory. Redirects are refused, and admin
mTLS credentials are never used for the public workload URL. Only GETs are sent.
No raw Realm, workload environment, log text, secret values or keyring is saved.

```sh
python3 scripts/acceptance.py vm --inventory inventory.json --seconds 600 \
  --rate 5 --workers 2 --max-error-fraction 0 --max-p95-seconds 2 \
  --output evidence/baseline-001
```

Choose availability/latency thresholds **before** running. Duration is 60–3600
seconds, offered workload rate 1–20/s, workers 1–8, each HTTP timeout <=5 seconds,
and recovery observation 15–120 seconds. Rate is capped and missed slots are
skipped rather than backfilled. Diagnostics/identity/leader coverage probes run
in parallel every five seconds after the previous finite probe completes, in
addition to workload requests. Request/probe/cleanup deadlines can extend total
wall time beyond the requested load interval; reported actual duration includes
that overhead. Output must be new; prior evidence is never overwritten.

Baseline and bounded post-run recovery require every exact node/build, complete
diagnostics, zero observed log/audit failures, full leader collection coverage and
the independently pinned canary bytes. Every successful HTTP response during
load must match the canary hash; an integrity mismatch always fails. The summary
reports counts, availability failures, p50/p95/max latency, topology, requested
and actual times and SHA256s of `samples.json`; `recovered.json` retains the final
probe. Intermediate degraded coverage and fixed alert codes remain in samples.
This is HTTP/canary and collection evidence. It does not itself write Tasks,
rotate keys, measure CPU saturation, or prove writable storage exclusion.

## Controlled failure profile and cleanup

Add an explicit fault to the private inventory and pass `--allow-faults` only on
the dedicated lab. Hooks are operator-selected argv arrays, executed without an
implicit shell; the runner does not discover targets or use SSH automatically.
For a transient **controller API reachability** experiment, a libvirt operator
might review the following example on the hypervisor:

```json
"fault":{
  "label":"controller1-pause",
  "after_seconds":60,"duration_seconds":60,
  "expected_unreachable":["controller1"],
  "apply":["virsh","suspend","titanus-lab-controller1"],
  "revert":["virsh","resume","titanus-lab-controller1"]
}
```

Run the same VM command with `--allow-faults`, a new output directory and your
predeclared error budget. Hook execution has a 30-second timeout and process-group
termination on interruption. Revert is attempted in `finally` even when apply
fails/times out or the runner gets SIGINT/SIGTERM; its success/failure is recorded.
Keep an independent console/revert procedure. SIGKILL, client/hypervisor power
loss or failed revert cannot guarantee cleanup. Inspect the recorded result and
manually restore only the reviewed VM/network fault. The runner requires the
named host to be observed unreachable and complete post-recovery evidence; a
hook returning zero without the observed failure cannot pass. A pause does not
prove power fencing or safe writer exclusion; test those separately.

## Execute and retain the remaining matrix

Use a new evidence directory, one failure domain at a time and reviewed cleanup
for each row. Keep complete raw external observations as well as the runner's
JSON. Run the baseline and 10-minute tests first, then repeat selected scenarios
for 60 minutes and several seeds/topology placements. Benchmarks compare the
same hardware, candidate, workload, concurrency, limits and predeclared thresholds.

| Scenario | Required independent proof |
| --- | --- |
| Controller loss/partition/reboot | Remaining quorum writes; minority cannot acknowledge mutation; same committed history/PKI/Source bytes after rejoin; observed recovery time |
| Worker/Gateway loss and network partition | Lease expiry and original run identity; externally observed VIP withdrawal; no simultaneous owner; UNKNOWN Task held with no cross-node replay |
| Rotation/schedule under controller loss | All-host key readiness; pinned secret versions still functional; immutable rotation receipt; distinct confirmed retry IDs; failed Stop/revoke holds; external side-effect counter |
| Memory/PID/CPU pressure and collection | Actual cgroup counters, fresh complete assignments, missing-sample hold; scaling bounds/cooldown; reachable-node coverage and unreachable alerts; bounded journal growth |
| Storage stale-writer/OSD/MDS partition | Independently measured old-writer I/O denial **before** writable successor; complete raw/tree canary hashes and original Ceph object identities; fail closed on missing exclusion |
| Mixed-version install/upgrade/rollback | Real old/new native binaries on independent kernels; controller-first order; functional writes; interrupted preparation; old startup/installer refusal after floors |
| Whole-quorum/storage-byte DR | Frozen authenticated host/Ceph set stored independently; actual exclusion/return prevention; absent original roots; functional restored keys/Sources/ledger/local/RBD/CephFS bytes; preserved UNKNOWN and newest schema floor |
| Clock/boot/power matrix | Independently booted clocks/kernels, controlled skew/reboot/power loss with console evidence; no inferred success from HTTP health |

For DR follow [BACKUP_RECOVERY.md](BACKUP_RECOVERY.md)'s coordinated
`cluster-intent`, `ceph-export`, `cluster-create-host`, `cluster-seal`, verify,
host restore/proof, Ceph exclusion/import, completion/finalization sequence.
First verify every archive and record actual independently established exclusion.
Do not delete production data or simulate controller loss by deleting live roots.
Retain an encrypted independent copy of private backups and recovery signing keys.

**M4 restrictions remain:** restore requires the original retained Ceph
FSID/native image/subvolume identities. Replacement backend creation, retained
remote snapshot histories and monitor/OSD/MDS reconstruction are unsupported.
CephFS hardlinks/xattrs/ACLs/special metadata remain outside the accepted profile.
Signed fencing attestations record operator evidence; they do not measure power
exclusion. Private archives are unencrypted. Do not check the broad M4/final lab
boxes until the separately recorded supported scenarios have actually passed;
unsupported capabilities remain explicit scope limits.

## Release provenance

Each new candidate comes only from an all-five-successful exact-main push run.
The publisher now uses `verify-release.py` on both downloaded native packages
before creating/publishing the immutable tag. Record PR final head/run, expected
head merge, new main/run, publisher, tag/manifest SHA and both archive hashes in
issue #20. A source change restarts the full five-job CI; previous evidence does
not transfer to a different commit. Never publish a PR-run package as a release.
