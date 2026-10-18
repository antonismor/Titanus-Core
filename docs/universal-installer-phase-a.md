# Universal Installer — Phase A

This document records Phase A. The later [Phase B](universal-installer-phase-b.md)
enables the Native HA profile for fresh installations; Standard-to-HA migration
remains blocked.

Phase A builds on PR #45 (Community limit and Enterprise roadmap). It changes
source and CI only; it does not deploy to any Proxmox host or existing VM.

## Available flow

Run `titanus setup`, or choose **Create / configure Realm** in the main menu.

* **Standard Titanus Core:** 1–9 host nodes, with a shared root SSH username/port.
* **Advanced/Custom:** 1–9 nodes, editable capabilities and optional existing
  distributed Ceph configuration/provisioning through the existing guarded path.
* **Titanus Native HA Cluster:** visible but deliberately unavailable in Phase A.
  It cannot be enabled by editing the saved plan. Automatic Worker failover,
  validated fencing, split-brain prevention, safe failback and data-preserving
  Standard → Native HA migration must be completed before enabling this profile.

Each VM or physical server counts as one node. Applications, Units and Fleets do
not affect the limit. Enterprise displays **Soon Available**, with unlimited nodes
and a 90-day trial as planned capabilities, not an active entitlement.

The wizard validates and saves the versioned plan, then prepares the pinned
release offline. The root password is requested only if applying the prepared
plan, after fingerprint approval. Saved plans never contain passwords. Existing
plans without `installation.profile` remain supported. Existing running Realms
are not modified by these source changes; the PR #45 limitation on new Community
plans above nine nodes remains unchanged.

## SSH trust bootstrap

Prerequisites on the operator: OpenSSH client (`ssh`, `ssh-keyscan`, `ssh-keygen`)
and `sshpass`. Debian/Ubuntu package names: `openssh-client sshpass`.
Each node needs an existing SSH server permitting the initial password login,
Python 3 and the OpenSSH client. The chosen SSH account must have UID 0. No sshd
configuration or system password is changed by the installer.

1. Scan Ed25519 host keys without authenticating. Existing operator known_hosts
   pins (including hashed entries) and Realm installer pins are checked. A
   changed/revoked key aborts instead of being removed or silently accepted.
2. Display SHA256 fingerprints. The operator must verify them through a trusted
   console and type `TRUST`. Scanning alone does not establish host identity.
3. Read the common initial password with terminal echo disabled. Redirected
   password input is refused. `sshpass -d` receives the password on an inherited
   pipe; the password never enters argv, environment variables or temporary files.
4. Retain a private, Realm-scoped installer Ed25519 identity under
   `~/.ssh/titanus-installer/<Realm hash>/`, with 0700 directories and 0600 private
   files. Each node generates its **own** Ed25519 key in
   `~/.ssh/titanus/<Realm hash>/`. Private node keys never leave their host.
5. Append missing public keys without replacing existing authorized_keys entries.
   Disable agent, port and X11 forwarding and SSH user rc for the added keys.
   Keep peer host pins in the Realm-specific known_hosts file. Reject symlinks,
   cloned node keys and unsafe existing private-key files. Serialize updates and
   publish remote files with atomic replacement.
6. Test every ordered pair with `BatchMode=yes`, the explicit node identity,
   `IdentityAgent=none` and `StrictHostKeyChecking=yes`. Nine nodes require **72/72**
   successful directed checks. Three require 6/6. A single node has 0 cross-node
   pairs; installer key authentication to that node is still required.
7. Only a successful mesh allows the wizard to apply the prepared deployment.
   Realm preflight, file transfer and Ceph operations retain the same installer
   identity and host pins. Existing explicit deployment CLI paths retain their
   previous key-based behavior for compatibility.

To configure/verify SSH trust for a saved plan without deploying it:

```sh
titanus trust titanus-plan.json
```

SSH bootstrap changes only the selected accounts' SSH trust files. If an operation
fails partway, it reports failure and does not deploy. It preserves completed key
work for a retry; it does not delete existing keys, roll back other administrators'
changes, or claim a partial mesh is ready. Retrying retains generated identities
and avoids duplicate public-key lines. The original password must still be valid
for a retry. Realm renaming creates a new trust scope; key retirement is an
explicit future management operation, not an automatic deletion.

The operator key grants root shell access to selected hosts and must be protected
like other administrator SSH credentials. Native HA/fencing policy is a separate
security boundary; SSH mesh success does not prove HA or storage correctness.

## Validation and next phases

Unit tests exercise 1/2/9 nodes, all 72 directed pairs, one-way failure, invalid
inventories, changed pins, password redaction, symlinks and offline wizard plans.
An actual local Python/OpenSSH test checks key generation, idempotency and
preservation of existing keys. The build CI job also boots three isolated SSH
servers/accounts on the disposable runner and tests password bootstrap, six real
peer connections, retry and wrong-password failure. All five existing CI jobs
remain required before any merge; none are skipped or replaced.

Next: implement and validate Native HA exclusion/fencing and safe failback,
three-storage-member Ceph quorum/replication policy, and an explicit reversible
Standard → Native HA migration. No new HA or data-migration guarantee is made by
Phase A.
