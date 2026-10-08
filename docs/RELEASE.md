# Versioned native release and recovery

The current versioned line is **0.4.0-rc.3**, an explicit release candidate. It
contains the implemented native subsystems and their documented limits; passing
CI does not establish maturity equivalent to established orchestration platforms
or install anything on user servers.

## Build and packages

`make build` embeds a common version, exact Git revision, Linux architecture and
`titanus-state/v3` profile in all four binaries. Builds are static Go binaries
with trimpath and VCS stamping disabled (the revision is explicit). `make release`
produces a native-architecture `dist/titanus-VERSION-linux-ARCH.tar.gz` plus its
SHA-256 file. It includes the binaries, systemd units, verified installer,
manifest, internal checksum list, this document and licence. Archive owner/time
and gzip timestamps derive from the Git commit for reproducible metadata.

AMD64 and ARM64 release archives are built and executed on **native runners**.
CI verifies real runtime/isolation/storage tests, actual cgroup-driven scaling,
Task/secret execution and three-controller/two-execution-node failure integration.
The nodes in that integration are distinct network namespaces/processes/state
roots connected by veth/bridge, not five physical machines or five separate
kernels. It tests real mTLS API/agent/Source transfer, VXLAN service traffic,
rolling rollback, controller loss and execution-network loss/lease fencing.
Fabric gateway and Unit Ethernet identities are pinned to their IPv4 addresses,
including when an address is reused by a replacement Unit. Native integration
checks identity stability through rolling update and rollback. Node-originated
Service traffic uses node-only SNAT to return through the initiating node's
conntrack path; routed workload source addresses are preserved by that rule.
Physical host reboot, real switches and site-specific hardware validation remain
operator acceptance work. Failure results are never inferred from a cross-build.

Published release candidates come only from successful **main push** runs of
Titanus Core CI. The publication workflow downloads that run's native packages,
checks archive SHA-256 and manifest version/revision/architecture/profile, and
creates an immutable version tag/release at the tested main SHA. Existing tags
or releases with a different revision are rejected. No PR-run artifact is
published. Archives contain no user state, PKI, encryption keys or Source payloads.
The SHA files provide integrity; no external signing identity or KMS is claimed.

## Fresh install

On a dedicated supported Linux host, install prerequisites: cgroups v2, OverlayFS,
user/PID/mount/net namespaces, nftables/iproute2/nsenter, Landlock ABI >=3,
Python 3, Bash, tar and SHA-256 tools. Native Ceph clients and access credentials
are needed only for configured Ceph Disks. Reserve the mapped UID/GID range as
in ISOLATION.md. Kernel enforcement failures prevent workload exec.

Download the matching native package and its checksum from the same authenticated
release, verify the checksum against the release metadata, then extract:

```sh
sha256sum -c titanus-0.4.0-rc.3-linux-amd64.tar.gz.sha256
tar -xzf titanus-0.4.0-rc.3-linux-amd64.tar.gz
sudo bash titanus-0.4.0-rc.3-linux-amd64/scripts/install-release.sh \
  --bundle "$PWD/titanus-0.4.0-rc.3-linux-amd64"
```

The installer pins and verifies the complete checksum inventory, architecture,
state profile and identical version/revision in every binary. It stages a private
immutable release directory, verifies the copied bytes before executing them,
fsyncs files/directories and activates `/usr/local/lib/titanus/current` atomically.
Public binary and service paths are fixed symlinks into that selection. Installer
operations are serialized by flock. Existing unversioned/custom binaries or
service paths are rejected; migrate them explicitly instead of silently replacing
an existing installation. `--root /absolute/staging-root` exercises file activation
without controlling the host's services.

Configure `/etc/titanus/daemon.env` and `agent.env`, PKI, controller membership,
networking and pre-staged Source/remote Disk configuration using the existing
Realm deployment documentation as a configuration reference. The legacy
`realm deploy`/interactive SSH provisioner installs flat binaries and is not an
upgrade/configuration command for versioned hosts; it refuses those hosts before
staging or rewriting their PKI/configuration. Configure versioned hosts explicitly.
Provision secret keyrings separately as in
ORCHESTRATION.md. The installer creates directories, but never generates user
PKI, changes networking, provisions Ceph devices or starts a new unconfigured
cluster implicitly. Start the configured daemon/agent with systemctl.

## Upgrade and binary rollback

Keep a secured, quiesced backup of state, mapping ledger, controller membership,
Source/Disk/snapshot catalogs, data and separately backed-up PKI/keyrings. Storage
snapshots have offline/ownership requirements; see STORAGE_LIFECYCLE.md. Do not
roll back the UID mapping ledger independently of data.

Install the next compatible verified bundle with the same command. The installer
stops only active Titanus agent/daemon services, atomically changes the selected
release and keeps the prior selection. `KillMode=process` preserves native Unit
PID 1 and host monitors; changing the binaries does not terminate live workloads.
The installed runtime still enforces leases and startup/recovery rules. Service
restart failure attempts to restore the earlier binary selection and active
services. Service-start acceptance is not application readiness; inspect actual
metrics, diagnostics, health and assignments after every change.

```sh
sudo bash /usr/local/lib/titanus/current/scripts/install-release.sh --rollback
titanus version --json
titanus diagnostics
```

Rollback is **binary selection only**, never an automatic reversal of live Realm,
PKI, secret, Source or storage data. It is permitted only between installed
bundles with the same explicit supported state profile. In an HA cluster, perform
one controller at a time, maintain quorum and verify the leader/replicated state.
Full mixed-version wire/schema compatibility is not promised. This first release
has no prior publicly released compatible version; CI upgrades between separately
identified fixture builds with the same profile and verifies live-daemon service
restart and a retained Unit.

Old release directories are retained for operator-directed cleanup. Installer
selection failures do not delete them or discard persistent configuration/data.
To leave the versioned line for a legacy flat install or a changed schema,
perform an explicitly planned offline migration and restore; automatic downgrade
to pre-isolation/pre-storage ownership versions is unsupported.

## Candidate 2 storage boundary

Candidate 2 adds opt-in replicated Disk catalogs, exact native writer identities,
durable automatic remote fencing/transfer, node quarantine and cluster-owned
UID/GID mappings. See STORAGE_FAILOVER.md. The state profile is now v2: the
versioned installer deliberately rejects cross-profile activation/rollback,
including rc.1 to rc.2 and rc.2 to rc.3. A tested cross-version migration is phase-2 M5 work;
do not run old controller binaries against catalogs or new mapping state.
Same-profile native installation/upgrade/rollback is still tested using distinct
fixture version identities, not a claim of a mixed-version cluster upgrade.
The published rc.1 archive/tag is preserved. Final VM/site acceptance remains open.

The default Make build version comes from `internal/version/version.go`.
Packaging and publication compare every binary/archive against that exact
declaration and state profile. A custom `make VERSION=...` build is not a
publishable release; the native installer fixture alone explicitly permits its
`.install-test` version during staging. Published tags are never moved.
