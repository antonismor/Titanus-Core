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
one-time browser bootstrap fragment. After that first successful sign-in, the
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
