# Titanus Core Web Installer

The Titanus Core Web Installer is an optional browser front-end for the same
validated `titanus-plan/v2` installation model used by the ANSI/TUI installer.
It does not replace or fork the CLI/TUI deployment engine.

## Starting the installer

After installing a release that contains the Web Installer:

```sh
sudo tuigui
```

`tuigui` starts `titanus-installer-web.service`, detects a local
IPv4 address and prints a protected HTTPS URL in English. The URL contains a
protected browser bootstrap fragment. After that first successful sign-in, the
browser keeps an HttpOnly, Secure, SameSite=Strict installer session cookie, so
reopening the same `https://IP:8088/` address in that browser restores the
current status.

The service listens on port `8088` by default. It creates a local self-signed
HTTPS identity under `/var/lib/titanus/installer-web`; therefore a browser may
require explicit certificate confirmation on first use. The root SSH bootstrap
password is never written to the Plan, URL, environment, state file or verbose
log. It is held only in process memory long enough to establish the existing
Realm-scoped SSH trust and is then zeroed.

For the terminal-only setup path:

```sh
sudo tuicli
```

This directly executes the existing `titanus setup` workflow. The Web Installer
service is not required by `tuicli` and the TUI remains independently usable.

## UI and TUI parity

The browser workflow exposes the installation choices from `titanus setup`:

- Community Edition topology (1–9 Realm nodes)
- Standard, Native HA fresh-install and Advanced/Custom profiles
- install, same-profile upgrade and binary rollback where supported
- Realm/Fabric/Service addressing
- node identities, management/Fabric/Ceph addresses and Titanus capabilities
- SSH username/port and the in-memory initial bootstrap password
- full directed SSH trust verification after explicit fingerprint approval
- Ceph provisioning, networks, replication, capacity, RBD/CephFS/RGW policy and
  explicit OSD device selection
- Native HA fencing inventory and the same model validation
- exact release version/revision, AMD64/ARM64 archive paths and pinned SHA-256
- PKI directory, API/Raft ports, per-node subnet prefix, VXLAN ID and service
  activation choice
- offline validate/prepare-only mode or explicitly authorized live apply
- protected browser download of the validated `titanus-plan/v2` JSON for the same review/export purpose as the TUI Plan output

Enterprise is visible in the common installer shell as **Soon Available**. The
current Community build does not implement Enterprise entitlements or remove the
Community node ceiling.

## Progress, verbose output and reconnect behavior

The Deployment screen follows the light Titanus Enterprise Installer reference:
a six-stage navigation rail, installation summary, large progress bar, per-stage
status and a collapsible **Verbose Installation Output** terminal immediately
below the progress bar.

The HTTPD persists non-secret installer state to
`/var/lib/titanus/installer-web/state.json` and verbose events to
`/var/log/titanus/installer-web.log`. Closing, refreshing or temporarily losing
the browser does not stop an in-process deployment; reconnecting shows the
latest persisted percentage, node count, phase and verbose output.

If the installer *service process itself* is restarted during an active apply,
Titanus reports the last durable state as `interrupted`. It deliberately does
not replay a possibly destructive step automatically. The operator must inspect
the verbose output and revalidate before any retry.

## Safety boundaries

The browser calls the existing Titanus model validator, SSH trust implementation,
release verifier, `PrepareRelease` path and `RealmDeployer.ApplyPrepared`. It
never bypasses Community topology limits, Native HA fencing requirements,
checksum verification, preflight, Ceph device guards, schema compatibility or
versioned installation rules.

The web service is not automatically started or enabled by package installation. It is
started only when an administrator runs `tuigui` (or explicitly starts the
systemd unit). `tuicli` stops an idle Web Installer service before entering the
terminal-only setup and refuses to interrupt an active Web Installer deployment. Use it only from a trusted management network and verify the
browser certificate fingerprint out of band when appropriate.


## Live-test candidate from this branch

Build and package the exact checked-out commit on Linux AMD64 and Linux ARM64
with `make release`. Both archives and their published `.sha256` files must
refer to that same commit; do not mix the branch Web Installer with rc.17
binaries from another revision. The native CI jobs keep verified release
packages as `release-ubuntu-24.04` and `release-ubuntu-24.04-arm` artifacts.
These branch artifacts are test candidates, not a newly published stable release.

On the selected installer host, install the matching native bundle using the
included `scripts/install-release.sh`. This installs the launch commands and
service unit without starting a deployment. Run `sudo tuigui`, open its printed
protected URL, and confirm the light six-step installer. Start with **Validate
& Prepare Only** and the two verified archive paths/checksums; this mode does
not authenticate to or change the target hosts.

For the later authorized live test, revalidate, approve the independently
verified SSH fingerprints and press **Start Installation**. Refresh the page,
close/reopen it at the same IP, and temporarily disconnect the browser while
monitoring: the backend deployment must continue and the displayed progress
must recover. Collapsing verbose output or scrolling back in it must survive
status updates. A restart of the service itself preserves the checkpoint as
`interrupted`; another start requires explicit revalidation.

`tuicli` requests an authenticated, atomic idle confirmation from the loopback
Web API before stopping the Web service. Once accepted, the API refuses new
installation requests. If the service is busy or cannot confirm its state,
`tuicli` fails without stopping it. This prevents a new Web installation from
racing with a terminal-mode switch.

## Automated verification

The standard five-job CI includes Web Installer race tests, isolated launch
command checks and `scripts/ci-web-installer.cjs`. The browser smoke uses the
real HTTPS service and session cookie to verify access, Plan validation/export,
missing-archive failure reporting, saved progress recovery after service
restart, collapsible output and recovery after a failed status request. Its
checkpoint is a fixture; it does not perform a live installation or contact
the documentation-only node addresses. Browser evidence is kept in the
`web-installer-browser` workflow artifact. Full live deployment and HA
acceptance remain a separate step on explicitly selected hosts.
