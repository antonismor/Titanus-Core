# Native Unit security, profile v1

Every newly started Unit uses `titanus-default-v1`: `no_new_privs=true`, an
architecture-checked seccomp allowlist and no Linux capabilities by default.
These controls are part of Titanus itself and use Linux syscalls directly.

The audit baseline for this milestone is main `eae966f01afad1ca7a5c25dc99566a54b5b340a8`:
native runtime, Fabric, DNS, Service Fabric and ingress/egress Network Policies
already existed. None of those components are replaced by this milestone.

This consolidated implementation includes the work from PR #6: the `restricted`
profile API, numeric UID/GID configuration, read-only rootfs and non-root rootfs
traversal. Its earlier unconfined proposal is rejected by the hardened policy.
The seccomp denylist is strengthened to an allowlist and applied with TSYNC;
the helper must stay on its locked OS thread through exec.

## Application capabilities

An application needing a low-numbered listening port can request:

```sh
titanus unit create web --source web --fabric --capabilities NET_BIND_SERVICE -- /bin/web
```

Unit specifications and Fleet `template.security` accept the same JSON:

```json
{
  "seccomp": "titanus-default-v1",
  "no_new_privs": true,
  "capabilities": ["CAP_NET_BIND_SERVICE"]
}
```

Only CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, SETGID, SETUID and NET_BIND_SERVICE
are accepted. Each explicitly requested capability grants its usual Linux
privilege; request only those the workload needs. Names are normalized and
duplicates rejected. Unconfined seccomp, disabled no_new_privs, unknown profiles
and administrative capabilities are rejected through CLI, Fleet and agent paths.

All five capability sets (effective, permitted, inheritable, bounding, ambient)
are reduced to the requested set. Locked securebits disable UID 0's implicit
capability restoration and setuid fixups. Explicit ambient capabilities survive
ordinary exec. `no_new_privs` prevents privilege gains from setuid binaries and
file capabilities and cannot be unset.

For a non-root workload with an immutable root filesystem:

```sh
titanus unit create web --source web --fabric --uid 65534 --gid 65534 --read-only-rootfs -- /bin/web
```

Fleet creation supports the same flags. JSON fields are `profile: "restricted"`,
`run_as_uid`, `run_as_gid`, and `read_only_rootfs`. Supplementary groups are
cleared. UID/GID changes affect the locked exec thread before its capability
sets are narrowed. Read-only rootfs keeps separate tmpfs and Disk mounts usable;
applications must arrange ownership of their writable Disks themselves.

## Enforcement boundary

The parent/child barrier completes cgroup and Fabric attachment. A host monitor
starts namespace init, stays outside the Unit and waits for its exit independently
of titanusd. Init locks its OS thread, prepares namespaces, pivots into rootfs,
mounts proc/dev/tmp and brings loopback up. It applies security and re-execs its
already-open executable on that same thread. Every new supervisor thread then
inherits the restricted identity, capabilities, no_new_privs and seccomp.

The restricted supervisor remains PID 1, starts the application in a process
group and owns all wait4 calls, including adopted orphans. It acknowledges
startup only after the application's exec succeeds. Errors, premature EOF and
timeouts produce FAILED and clean up the Unit. ACTIVE confirms exec, not
application readiness. See [RUNTIME_LIFECYCLE.md](RUNTIME_LIFECYCLE.md).

Existing persisted specs without security fields receive hardened defaults on
their next start. Stop and restart Units that were already running at upgrade;
the policy is not retroactively applied to their processes. Unit inspect shows
the persisted policy; `/proc/<pid>/status` is authoritative for kernel state.

## Seccomp coverage

Native Linux AMD64 and ARM64 are supported. Other architectures fail closed.
The explicit syscall tables cover ordinary file I/O, sockets, memory, signals,
threads, process lifecycle, clocks and IPC. Unlisted and newly introduced
syscalls return EPERM. Foreign ABIs are killed; x32-tagged syscalls are killed,
and the historical AMD64 512–547 x32 range is not allowed.

Mount operations, namespace entry/creation, kernel module loading, reboot,
ptrace, keyring operations, BPF, performance events, io_uring and new mount API
calls are outside the allowlist. `clone` allows ordinary processes/threads but
rejects namespace flags. `clone3` returns ENOSYS because classic BPF cannot
inspect its pointer-based flags; libc may fall back to checked `clone`.

Mapped user namespaces, mandatory cgroup v2 device filtering, protected proc and
fail-closed Landlock now extend this profile. See [ISOLATION.md](ISOLATION.md) for
kernel requirements, mapped Disk ownership, legacy upper-layer migration and
precise LSM limits. Privileged native runtime CI executes on AMD64 and ARM64.
Workloads needing additional syscalls require a reviewed profile update.

## Validation

Ordinary tests evaluate both architectures' cBPF bytecode, defaults, persistence,
Fleet propagation, policy rejection and startup success/error/timeout handling.
Privileged CI executes the filter in Linux and verifies capability sets after
exec, denied syscalls, clone3 fallback, no_new_privs irreversibility and a real
setuid helper running as UID 1000 without elevation. The native Unit smoke runs
the same probe inside the existing OverlayFS/cgroup/Fabric/Disk path and checks
the live workload's proc status. A nonexistent workload executable must leave
FAILED rather than ACTIVE.

References: [Linux seccomp filter API](https://docs.kernel.org/userspace-api/seccomp_filter.html),
[Linux no_new_privs](https://docs.kernel.org/userspace-api/no_new_privs.html),
[Linux capabilities](https://man7.org/linux/man-pages/man7/capabilities.7.html).
