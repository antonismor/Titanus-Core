# Native runtime lifecycle and recovery

Titanus uses three processes: titanusd (or a CLI caller), an independent host
monitor, and restricted namespace PID 1. The application is PID 1's child.
PID 1 forwards termination signals to the workload process group and reaps
all adopted children with wait4. When the main workload exits, PID 1 kills its
remaining process group and exits with the workload's status. The kernel tears
down remaining descendants when namespace PID 1 exits.

The host monitor holds no daemon lifetime dependency. It waits for namespace
init and writes a durable, run-specific exit record outside the Unit. Host
state file descriptors are never passed into the Unit. Successful exit is
STOPPED, nonzero exit is FAILED; inspect reports exit_code and exit_signal.
An explicit stop keeps STOPPED while retaining the workload's exit code.

State writes sync the file and the directory around atomic rename. Runtime
mutations and inspection share a flock across CLI and daemon processes. Process
identity consists of kernel boot_id and /proc starttime; zombies are not active.
Stop opens a pidfd before checking identity and delivers signals through that
descriptor. A reused PID or a PID from another boot cannot authorize a signal.
Older live Units can be adopted only if their PID is present in their private
Unit cgroup. Linux with pidfd support is required; there is no unsafe kill fallback.

On daemon start, recovery validates persisted Units and detaches dead runtime
resources. Live Units retain their namespaces and identity. Node reboot
invalidates old process identities. Unexpected host monitor loss may leave an
unknown exit code; Titanus never invents an exit result from a missing record.
This milestone does not automatically restart completed workloads; restart
policy and application health are separate milestones.

Leases persist original expiry times and boot ID. Daemon restart does not extend
them; reboot invalidates them. Expired leases stop their Unit before removal.
Failed stops remain recorded and retry. Renewal and expiry serialize, and
renewing an expired lease requires fencing its previous Unit first.

The installed/generated daemon systemd unit uses KillMode=process so a daemon
restart leaves host monitors alive. Workload processes are moved into separate
Titanus cgroups; stop Units explicitly before a full runtime shutdown or upgrade
that changes their security policy. ACTIVE means exec was confirmed, not health.

CI runs native AMD64 and ARM64 privileged security/runtime checks, orphan
reaping, signal forwarding, exit propagation, daemon kill/restart adoption and
resource cleanup. Unit tests cover PID reuse, reboot identity, stale exit
records, lease restoration and retry after failed fencing. True host reboot
and multi-node failure testing remain in the release milestone tracked in #8.
