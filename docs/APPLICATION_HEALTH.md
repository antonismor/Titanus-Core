# Application health and restart policy

ACTIVE confirms successful application exec. `state.ready` is the availability
decision. With a readiness probe it starts false, becomes true after the success
threshold, and becomes false after the failure threshold. The reconciler marks
an assignment ACTIVE only when its runtime is ACTIVE and ready; Route, Service
Fabric and DNS backend selection already use that assignment state.

HTTP and TCP probes target `127.0.0.1` inside the Unit's network namespace.
Each probe uses a separate native helper, pins the namespace against PID reuse,
drops to UID/GID 65534 with no capabilities, sets no_new_privs/seccomp, then
re-execs before network I/O. It never enters the daemon's runtime thread or uses
the host network. HTTP checks accept 200–399, do not follow redirects, use no
proxy, and bound timeout/header/body reads. Exec and HTTPS probes are not part
of this version. The daemon runs at most eight probe helpers concurrently.

```sh
titanus unit create web --source web --readiness http://127.0.0.1:8080/ready \
  --liveness http://127.0.0.1:8080/live --restart on-failure -- /bin/web
```

Unit and Fleet creation accept these flags and `--health-config FILE` for the
full `health` JSON object. Defaults are a 5s probe interval, 2s timeout, one
success and three failures. Initial delay, thresholds and timeouts are bounded
and validated. Probes do not require Fabric, published ports or host tooling.

Restart policies are `never`, `on-failure`, and `always`. Standalone Units
default to never; Fleets default to always. Liveness failure terminates the
execution and is a failure even if the application exits zero during shutdown.
Backoff starts at 2s, doubles to a 60s cap, and survives daemon restart. Optional
max_restarts bounds attempts. Ten minutes of stable runtime resets the restart
streak. Explicit operator Start clears the streak; explicit Stop and expired
lease fencing clear desired_running, preventing background restart.

The controller's repeated start requests honor stored backoff and restart
limits. Probe results carry the execution run ID and cannot update a newer
execution. A Unit without probes remains ready after successful exec, preserving
legacy behavior. Legacy health defaults are persisted during matching Ensure;
adding probes requires an updated Fleet generation or a newly specified Unit.

Health supervision requires titanusd to run. Workloads survive its restart, but
probes pause while the daemon is absent. Probe scheduling is bounded by configured
intervals, timeouts and worker capacity; large installations must budget enough
probe capacity. CI verifies native isolated probes, initial unready state,
liveness termination, restart/key execution change, readiness recovery, daemon
restart persistence and operator-stop suppression on AMD64 and ARM64.
