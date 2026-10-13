# Proxmox Command Center gateway

The optional Python gateway serves Titanus Command Center through an existing Proxmox browser session. Titanus still uses an operator admin certificate for its backend mTLS connection. Keep that private key local to the gateway.

Run on Python 3.10 or later. Python 3.13 enables stricter certificate checks which reject some legacy Proxmox root CAs without a Key Usage extension. The gateway instead trusts the exact certificate in `/etc/pve/local/pve-ssl.pem`, with strict verification and hostname checks enabled. It does not disable TLS validation. Restart the gateway after Proxmox renews its certificate.

Set these environment variables before running `python3 scripts/proxmox-command-center-gateway.py`:

| Variable | Meaning |
| --- | --- |
| TITANUS_WEBUI_HOST | Explicit management IP to bind; default is loopback |
| TITANUS_WEBUI_PORT | HTTPS port; default 8443 |
| TITANUS_WEBUI_ORIGIN | Browser origin; defaults to HTTPS host and port |
| TITANUS_WEBUI_SUBNET | Allowed management subnet; defaults to loopback /32 |
| TITANUS_WEBUI_BACKENDS | Comma-separated HTTPS controller origins |
| TITANUS_WEBUI_OPERATOR_DIR | Directory containing ca.crt, admin.crt, admin.key |
| TITANUS_WEBUI_PVE_ORIGIN | Internal Proxmox API origin, whose hostname must match the pinned certificate |
| TITANUS_WEBUI_LOGIN_URL | Browser Proxmox login URL using the same host as the gateway |
| TITANUS_WEBUI_PVE_CERT | Exact local PVE server certificate; defaults to /etc/pve/local/pve-ssl.pem |
| TITANUS_WEBUI_CERT / TITANUS_WEBUI_KEY | HTTPS server certificate/key |

Use the same browser hostname for Proxmox login and the gateway so the existing PVEAuthCookie is available on both ports. Root-level Sys.Modify or Permissions.Modify is required. Anonymous and invalid tickets fail closed. Writes require the exact configured browser Origin.

The gateway discovers the current controller leader. Reads can retry another controller. Mutations only retry a 503 explicitly marked X-Titanus-Rejected=true; a transport failure or ambiguous 503 is returned for inspection, since replaying an uncertain write could create a duplicate action.

A verified Gateway VIP withdrawal retains a healthy former owner as standby, allowing repeated transfers. A power-fenced or unhealthy former owner remains excluded.

Validation: the existing five CI jobs include Python regression tests for real TLS with a legacy CA, incorrect certificate and hostname rejection, host permissions, failed authentication transport, and prevention of uncertain mutation replay. A deployed private-network browser login remains a separate acceptance check; CI browser tests exercise native Titanus mTLS.
