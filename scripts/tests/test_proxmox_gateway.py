import importlib.util
import io
import json
import pathlib
import ssl
import subprocess
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from email.message import Message
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

SOURCE = pathlib.Path(__file__).resolve().parents[1] / "proxmox-command-center-gateway.py"
spec = importlib.util.spec_from_file_location("proxmox_gateway", SOURCE)
gateway = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gateway)

class Response(io.BytesIO):
    def __init__(self, data, status=200, rejected=False):
        super().__init__(json.dumps(data).encode())
        self.status = status
        self.headers = Message()
        self.headers["Content-Type"] = "application/json"
        if rejected:
            self.headers["X-Titanus-Rejected"] = "true"

class Permissions:
    def __init__(self, data):
        self.data = data
        self.calls = 0
    def open(self, request, timeout):
        self.calls += 1
        return Response(self.data)

class GatewayAuthentication(unittest.TestCase):
    def test_valid_ticket_needs_host_privileges(self):
        for data, expected in [
            ({"data": {"/": {"Sys.Modify": 1}}}, True),
            ({"data": {"/": {"Permissions.Modify": 1}}}, True),
            ({"data": {"/vms/100": {"Sys.Modify": 1}}}, False),
            ({"data": {"/": {"VM.Audit": 1}}}, False),
        ]:
            with self.subTest(data=data), patch.object(gateway, "pve_client", Permissions(data), create=True):
                self.assertEqual(gateway.authenticated("PVEAuthCookie=test-ticket"), expected)
        client = Permissions({"data": {"/": {"Sys.Modify": 1}}})
        with patch.object(gateway, "pve_client", client, create=True):
            self.assertFalse(gateway.authenticated(""))
            self.assertEqual(client.calls, 0)

    def test_transport_failure_denies_access(self):
        class Unavailable:
            def open(self, *args, **kwargs):
                raise urllib.error.URLError("TLS validation failed")
        with patch.object(gateway, "pve_client", Unavailable(), create=True):
            self.assertFalse(gateway.authenticated("PVEAuthCookie=test-ticket"))

class GatewayRetries(unittest.TestCase):
    def test_unknown_writes_are_not_replayed(self):
        scenarios = [("POST", "transport", 502, 1), ("POST", "503", 503, 1),
                     ("POST", "rejected", 200, 2), ("GET", "transport", 200, 2),
                     ("GET", "503", 200, 2)]
        for method, failure, expected_status, expected_calls in scenarios:
            with self.subTest(method=method, failure=failure):
                class Backend:
                    calls = 0
                    def open(self, request, timeout):
                        self.calls += 1
                        if self.calls == 1:
                            if failure == "transport":
                                raise urllib.error.URLError("lost reply")
                            return Response({"error": "unavailable"}, 503, failure == "rejected")
                        return Response({"ok": True})
                backend = Backend()
                server = ThreadingHTTPServer(("127.0.0.1", 0), gateway.Handler)
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                try:
                    with patch.object(gateway, "authenticated", return_value=True), \
                         patch.object(gateway, "BACKENDS", ["https://a", "https://b"]), \
                         patch.object(gateway, "leader_url", return_value="https://a"), \
                         patch.object(gateway, "titanus_client", backend, create=True):
                        body = b"{}" if method == "POST" else None
                        request = urllib.request.Request(
                            f"http://127.0.0.1:{server.server_port}/v1/realm/fleets",
                            body, {"Origin": gateway.FRONT_ORIGIN}, method=method)
                        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                        try:
                            response = opener.open(request, timeout=3)
                        except urllib.error.HTTPError as error:
                            response = error
                        with response:
                            self.assertEqual(response.status, expected_status)
                        self.assertEqual(backend.calls, expected_calls)
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join(3)

class ProxmoxLeafTLS(unittest.TestCase):
    def test_strict_leaf_pin_accepts_legacy_ca_but_rejects_wrong_cert_and_hostname(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            def openssl(*args):
                subprocess.run(["openssl", *args], cwd=root, check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            (root / "ca.cnf").write_text("""[req]
distinguished_name=dn
x509_extensions=ca
prompt=no
[dn]
CN=Legacy Proxmox fixture
[ca]
basicConstraints=critical,CA:true
subjectKeyIdentifier=hash
""")
            openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-config", "ca.cnf", "-keyout", "ca.key", "-out", "ca.pem")
            (root / "leaf.cnf").write_text("""basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
""")
            for name, serial in [("server", "1"), ("wrong", "2")]:
                openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj",
                        "/CN=localhost", "-keyout", name + ".key", "-out", name + ".csr")
                openssl("x509", "-req", "-in", name + ".csr", "-CA", "ca.pem",
                        "-CAkey", "ca.key", "-set_serial", serial, "-days", "1",
                        "-extfile", "leaf.cnf", "-out", name + ".pem")
            class Handler(BaseHTTPRequestHandler):
                def do_GET(self):
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b"verified")
                def log_message(self, *args):
                    pass
            server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            tls.load_cert_chain(root / "server.pem", root / "server.key")
            server.socket = tls.wrap_socket(server.socket, server_side=True)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            def fetch(context, host="localhost"):
                opener = urllib.request.build_opener(
                    urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context))
                with opener.open(f"https://{host}:{server.server_port}", timeout=3) as response:
                    return response.read()
            try:
                legacy = ssl.create_default_context(cafile=root / "ca.pem")
                legacy.verify_flags |= ssl.VERIFY_X509_STRICT
                with self.assertRaises(urllib.error.URLError):
                    fetch(legacy)
                pinned = gateway.pinned_pve_context(str(root / "server.pem"))
                self.assertTrue(pinned.check_hostname)
                self.assertEqual(pinned.verify_mode, ssl.CERT_REQUIRED)
                self.assertEqual(fetch(pinned), b"verified")
                with self.assertRaises(urllib.error.URLError):
                    fetch(gateway.pinned_pve_context(str(root / "wrong.pem")))
                with self.assertRaises(urllib.error.URLError):
                    fetch(pinned, "127.0.0.1")
            finally:
                server.shutdown()
                server.server_close()
                thread.join(3)

if __name__ == "__main__":
    unittest.main()
