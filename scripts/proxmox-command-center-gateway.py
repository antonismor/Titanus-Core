#!/usr/bin/python3
"""Browser-safe, Proxmox-authorized gateway for Titanus's mTLS Command Center.
Uses the existing local Titanus operator certificate. Never exposes it to clients.
"""
import http.server
import os
import ipaddress
import json
import pathlib
import ssl
import urllib.parse
import urllib.request
import urllib.error

HOST = os.environ.get("TITANUS_WEBUI_HOST", "127.0.0.1")
PORT = int(os.environ.get("TITANUS_WEBUI_PORT", "8443"))
FRONT_ORIGIN = os.environ.get("TITANUS_WEBUI_ORIGIN", f"https://{HOST}:{PORT}")
PVE = os.environ.get("TITANUS_WEBUI_PVE_ORIGIN", "https://pve:8006")
PVE_LOGIN_URL = os.environ.get("TITANUS_WEBUI_LOGIN_URL", f"https://{HOST}:8006")
LAB = pathlib.Path(os.environ.get("TITANUS_WEBUI_OPERATOR_DIR", "/etc/titanus-webui/operator"))
BACKENDS = [u.strip() for u in os.environ.get("TITANUS_WEBUI_BACKENDS", "").split(",") if u.strip()]
SUBNET = ipaddress.ip_network(os.environ.get("TITANUS_WEBUI_SUBNET", "127.0.0.1/32"))
def pinned_pve_context(certificate):
    """Trust the local PVE leaf with strict TLS and hostname verification."""
    ctx = ssl.create_default_context(cafile=certificate)
    ctx.verify_flags |= ssl.VERIFY_X509_PARTIAL_CHAIN | ssl.VERIFY_X509_STRICT
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    return ctx

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None

def make_opener(context):
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=context),
        NoRedirect(),
    )

def configure_clients():
    global pve_client, titanus_client
    if not BACKENDS or any(urllib.parse.urlsplit(u).scheme != "https" for u in BACKENDS):
        raise ValueError("TITANUS_WEBUI_BACKENDS requires HTTPS controller origins")
    pve_ctx = pinned_pve_context(os.environ.get("TITANUS_WEBUI_PVE_CERT", "/etc/pve/local/pve-ssl.pem"))
    titanus_ctx = ssl.create_default_context(cafile=str(LAB / "ca.crt"))
    titanus_ctx.load_cert_chain(str(LAB / "admin.crt"), str(LAB / "admin.key"))
    pve_client = make_opener(pve_ctx)
    titanus_client = make_opener(titanus_ctx)

def authenticated(cookie):
    if "PVEAuthCookie=" not in cookie:
        return False
    req = urllib.request.Request(
        PVE + "/api2/json/access/permissions",
        headers={"Cookie": cookie, "Accept": "application/json"},
    )
    try:
        with pve_client.open(req, timeout=5) as resp:
            payload = json.load(resp)
            info = payload.get("data", {}) if isinstance(payload, dict) else {}
            if not isinstance(info, dict):
                print("Titanus WebUI auth: unexpected Proxmox permissions payload", flush=True)
                return False
            # Require elevated host-level rights, not merely a valid ticket.
            perms = info.get("/", {})
            if not isinstance(perms, dict):
                return False
            return bool(perms.get("Sys.Modify") or perms.get("Permissions.Modify"))
    except (ValueError, urllib.error.URLError, TimeoutError, TypeError, AttributeError) as exc:
        print("Titanus WebUI auth validation failed: %s" % type(exc).__name__, flush=True)
        return False

def leader_url():
    # Local admin mTLS diagnostics reveal the leader even on followers.
    for base in BACKENDS:
        try:
            with titanus_client.open(base + "/v1/realm/consensus",timeout=3) as resp:
                obj = json.load(resp)
            origin = obj.get("leader_api", "")
            if origin in BACKENDS: return origin
        except (ValueError,urllib.error.URLError,TimeoutError):
            continue
    return BACKENDS[0]

class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "TitanusWebGateway/1.0"
    sys_version = ""
    def log_message(self, format, *args):
        # Standard access logging deliberately excludes cookies.
        if not self.path.startswith("/v1/"):
            super().log_message(format,*args)

    def send_data(self,status,data=b"",typ="text/plain; charset=utf-8",extra=None):
        self.send_response(status)
        self.send_header("Content-Type",typ)
        self.send_header("Content-Length",str(len(data)))
        self.send_header("Cache-Control","no-store")
        self.send_header("X-Content-Type-Options","nosniff")
        self.send_header("X-Frame-Options","DENY")
        self.send_header("Referrer-Policy","no-referrer")
        if extra:
            for k,v in extra.items(): self.send_header(k,v)
        self.end_headers()
        if self.command != "HEAD": self.wfile.write(data)

    def do_GET(self): self.handle_request()
    def do_POST(self): self.handle_request()
    def do_DELETE(self): self.handle_request()
    def do_PUT(self): self.handle_request()

    def handle_request(self):
        try:
            if ipaddress.ip_address(self.client_address[0]) not in SUBNET:
                return self.send_data(403,b"Management subnet only")
            if not authenticated(self.headers.get("Cookie","")):
                return self.send_data(401,
                    ("Please sign in to Proxmox at " + PVE_LOGIN_URL + " then refresh this page.").encode(),
                    "text/plain; charset=utf-8")
            path = urllib.parse.urlsplit(self.path).path
            if path == "/":
                return self.send_data(302,b"",extra={"Location":"/command-center/"})
            permitted = (
                path in ("/command-center","/command-center/","/command-center/app.js","/command-center/style.css",
                         "/v1/events","/v1/metrics","/v1/diagnostics") or path.startswith("/v1/realm/")
            )
            if not permitted: return self.send_data(404,b"Not found")
            if self.command != "GET":
                if self.headers.get("Origin") != FRONT_ORIGIN:
                    return self.send_data(403,b"Invalid request origin")
                if path.startswith("/command-center") or path in ("/v1/events","/v1/metrics","/v1/diagnostics"):
                    return self.send_data(405,b"Read-only endpoint")
            size = int(self.headers.get("Content-Length","0"))
            if size < 0 or size > (1<<20):
                return self.send_data(413,b"Request too large")
            body = self.rfile.read(size) if size else None
            leader = leader_url()
            candidates = [leader] + [u for u in BACKENDS if u != leader]
            for base in candidates:
                headers={"Accept":self.headers.get("Accept","application/json"),
                         "Origin":base}
                if body is not None:
                    headers["Content-Type"]=self.headers.get("Content-Type","application/json")
                req = urllib.request.Request(base + self.path, body, headers=headers, method=self.command)
                try:
                    resp = titanus_client.open(req,timeout=8)
                except urllib.error.HTTPError as e:
                    resp = e
                except (urllib.error.URLError,TimeoutError):
                    if self.command != "GET":
                        return self.send_data(502,b"Request outcome unknown; inspect state before retrying")
                    continue
                with resp:
                    status = resp.status
                    data = resp.read(4<<20)
                    if status == 503 and (self.command == "GET" or resp.headers.get("X-Titanus-Rejected") == "true"):
                        continue
                    h={}
                    for key in ("Content-Security-Policy","ETag","Last-Modified"):
                        if resp.headers.get(key): h[key]=resp.headers[key]
                    return self.send_data(status,data,resp.headers.get("Content-Type","application/json"),h)
            return self.send_data(503,b"Titanus Realm leader unavailable")
        except (ValueError,KeyError):
            return self.send_data(400,b"Invalid request")
        except Exception:
            return self.send_data(502,b"Titanus gateway error")

if __name__=="__main__":
    configure_clients()
    server = http.server.ThreadingHTTPServer((HOST,PORT),Handler)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(os.environ.get("TITANUS_WEBUI_CERT", "/etc/titanus-webui/https.crt"), os.environ.get("TITANUS_WEBUI_KEY", "/etc/titanus-webui/https.key"))
    server.socket = ctx.wrap_socket(server.socket,server_side=True)
    server.serve_forever()

