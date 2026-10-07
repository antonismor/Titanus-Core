# Native Unit security, profile v1

Every newly started Unit uses `titanus-default-v1`: `no_new_privs=true`, an
architecture-checked seccomp allowlist and no Linux capabilities by default.
These controls are part of Titanus itself and use Linux syscalls directly.

The audit baseline for this milestone is main `eae966f01afad1ca7a5c25dc99566a54b5b340a8`:
native runtime, Fabric, DNS, Service Fabric and ingress/egress Network Policies
already existed. None of those components are replaced by this milestone.

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

## Enforcement boundary

The existing parent/child barrier first completes cgroup and Fabric attachment.
The helper locks its OS thread, prepares namespaces, pivots into the rootfs,
mounts proc/dev/tmp and brings loopback up. Only then does it apply security and
execute the workload on the same thread. Seccomp TSYNC covers all helper threads.

The startup status descriptor is CLOEXEC. The parent requires a pre-exec marker
and closure of the descriptor before reporting ACTIVE. Setup/hardening/exec
errors, missing markers and timeouts produce FAILED, kill/reap the helper and
clean up Fabric, mounts and cgroups. The marker confirms initialization; this
protocol is not an application health check and workloads can exit after exec.

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

The profile is an initial general workload policy, not a complete sandbox.
Allowed ioctl/prctl operations still depend on kernel privilege checks. Units
still run as UID 0 without user namespaces. Device cgroup filtering, LSM policy,
non-root identity configuration and proc masking remain separate milestones.
ARM64 is cross-built and its filter logic tested; privileged runtime smoke
currently runs on AMD64. Workloads needing additional syscalls require a reviewed
profile update, rather than disabling enforcement.

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
