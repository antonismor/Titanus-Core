# API roles and certificate lifecycle

The cluster listener requires TLS 1.3 and a client certificate signed by its
Realm CA. Its single OrganizationalUnit is the CA-signed role. An unverified,
role-less, unknown-role, expired or revoked identity is denied. A nil TLS field
does not authorize management. Only the privileged Unix listener explicitly
injects local administrator authority; retain its filesystem permissions.

| Role | Allowed operations |
| --- | --- |
| node | Fabric/Realm state and public CRL reads, its own registration/Pulse, renewal of its own certificate |
| controller | Node runtime/Source/lease control, Realm reads, its own registration/Pulse and certificate renewal |
| admin | Desired Fleet/Route/Policy changes and all management operations, including revocation |

A Node certificate cannot impersonate another Node or claim CONTROL capability.
Node state responses omit Fleet environment values and assignment lease tokens.
Cross-origin browser requests are rejected. Future endpoints must retain the
authorization wrapper and an explicit role decision.

## Enrollment and migration

Realm init and deployment issue controller certificates for control nodes and
node certificates for execution nodes. Deployment distributes the signed
`ca.crl` with the CA certificate; the CA key remains on the primary controller.
Older role-less certificates must be reissued before starting cluster services:

```sh
titanus identity issue --pki-dir /etc/titanus/pki --realm LAB --id worker01 --role node --address 10.0.0.11
titanus identity issue --pki-dir /etc/titanus/pki --realm LAB --id control01 --role controller --address 10.0.0.10
titanus identity issue --pki-dir /etc/titanus/pki --realm LAB --id operator --role admin
```

Configure the returned certificate/key paths together; they must share a bundle
directory. Leaf lifetime defaults to 24 hours (accepted issuance range 1h–30d).
New leaf issuance uses a fresh key and serial in an immutable directory, with
an atomic current pointer. Admin leaves have only client-auth EKU. Reissuing a
leaf does not silently revoke its previous serial before replacement deployment.

## Renewal and revocation

Agents renew with less than eight hours remaining. The agent generates its
replacement private key locally and submits a signed CSR to the controller's
`POST /v1/identity/renew`. The controller copies identity, role and SANs from the
authenticated current certificate, ignoring expanded CSR claims. Certificate
and key rotation uses an atomic shared pointer; TLS reloads the current bundle
at each handshake. CLI `identity renew` supports operator/admin renewal too.

```sh
titanus identity revoke --pki-dir /etc/titanus/pki --realm LAB --serial HEX_SERIAL
titanus identity refresh-crl --pki-dir /etc/titanus/pki --realm LAB
```

Admins can also POST `{"serial":"HEX_SERIAL"}` to `/v1/identity/crl`; an empty
serial refreshes the cumulative list. The controller maintains the signed CRL
before expiry. Agents fetch `/v1/identity/crl` on each Pulse, validate its CA
signature, time window and monotonic sequence, and install it durably. CRL
propagation is bounded by the configured Pulse interval while connectivity is
healthy. On partition, existing valid CRLs remain in effect until expiry, then
authentication fails closed. Immediate disconnected-node revocation requires
out-of-band CRL delivery or network fencing; it is not promised by this API.

Every server request rechecks its peer's current certificate and CRL, including
keepalive connections. Agent/controller clients establish fresh TLS connections
so certificate/CRL changes are checked on each request. Expired or revoked
certificates cannot self-renew; operator reenrollment is required. A node absent
longer than the CRL validity window needs a fresh signed CRL delivered before
its cluster TLS client can start. CA replacement/rotation is a separate trust
rollout; this milestone rotates leaf certificates, not trust anchors.

Tests exercise real TLS connections, revocation on an existing connection,
key/serial renewal, CSR role/identity/SAN escalation rejection, foreign/stale CRL
rejection, API role gates and local-versus-TCP authorization. Native runtime
security and lifecycle CI continue on both AMD64 and ARM64.
