import argparse
import hashlib
import importlib.util
import json
import http.server
import pathlib
import tempfile
import threading
import time
import unittest
import urllib.request
from unittest import mock

SCRIPTS = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('acceptance', SCRIPTS / 'acceptance.py')
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class Pool:
    def __init__(self, **kwargs): pass
    def __enter__(self): return self
    def __exit__(self, *args): pass
    def map(self, function, values): return map(function, values)
    def submit(self, function, *args): return mock.Mock(exception=lambda: None)
    def shutdown(self, **kwargs): pass


class Event:
    def wait(self, *args): pass
    def set(self): pass
    def is_set(self): return False


class BoundedHTTP(unittest.TestCase):
    def test_redirect_refused_and_slow_response_has_total_budget(self):
        requests = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_GET(self):
                requests.append(self.path)
                if self.path == '/redirect':
                    self.send_response(302); self.send_header('Location', '/target'); self.end_headers()
                    return
                if self.path == '/slow':
                    self.send_response(200); self.send_header('Content-Length', '100'); self.end_headers()
                    for _ in range(100):
                        try:
                            self.wfile.write(b'x'); self.wfile.flush()
                        except (BrokenPipeError, ConnectionResetError):
                            return
                        time.sleep(0.01)
                    return
                self.send_response(200); self.end_headers(); self.wfile.write(b'EXPECTED')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            url = 'http://127.0.0.1:' + str(server.server_port)
            opener = urllib.request.build_opener(acceptance.NoRedirect())
            status, data, _ = acceptance.get(opener, url + '/redirect', 1)
            self.assertEqual(status, 302)
            self.assertNotIn('/target', requests)
            self.assertEqual(acceptance.get(opener, url + '/target', 1)[:2], (200, b'EXPECTED'))
            begin = time.monotonic()
            self.assertEqual(acceptance.get(opener, url + '/slow', 0.08)[0], 0)
            self.assertLess(time.monotonic() - begin, 0.5)
        finally:
            server.shutdown(); server.server_close(); thread.join()


class VMAdmissionAndCleanup(unittest.TestCase):
    def fixture(self, directory):
        sha = '1' * 40
        ids = ['c0', 'c1', 'c2', 'w0', 'w1']
        build = {'revision': sha, 'arch': 'amd64', 'os': 'linux'}
        nodes = []
        for i, node in enumerate(ids):
            fact = {'format': 'titanus-host-facts/v1', 'node_id': node, 'build': build,
                    'cgroup_v2': True, 'boot_id_sha256': str(i) * 64}
            (directory / (node + '.json')).write_text(json.dumps(fact))
            nodes.append({'id': node, 'role': 'controller' if i < 3 else 'worker',
                          'api': f'https://10.180.0.{i+11}:9443', 'facts': node + '.json'})
        key = directory / 'private.key'
        key.write_bytes(b'fixture-only')
        key.chmod(0o600)
        inv = {'format': 'titanus-acceptance-inventory/v1', 'revision': sha, 'realm': 'LAB',
               'ca': 'ca', 'certificate': 'certificate', 'key': key.name, 'nodes': nodes,
               'canary': {'url': 'http://10.180.0.100/integrity', 'sha256': hashlib.sha256(b'EXPECTED').hexdigest()},
               'fault': {'label': 'fixture', 'apply': ['fixture-apply'], 'revert': ['fixture-revert'],
                         'expected_unreachable': ['c0'], 'after_seconds': 5, 'duration_seconds': 5}}
        path = directory / 'inventory.json'
        path.write_text(json.dumps(inv)); path.chmod(0o600)
        return argparse.Namespace(inventory=str(path), allow_faults=True, timeout=1, seconds=60,
                                  rate=1, workers=1, recovery_seconds=15, max_error_fraction=0, max_p95_seconds=2)

    def responses(self, opener, url, timeout):
        node = ['c0', 'c1', 'c2', 'w0', 'w1'][int(url.split('//')[1].split(':')[0].split('.')[-1])-11]
        if url.endswith('/compatibility'):
            return 200, {'node_id': node, 'realm': 'LAB', 'capabilities': {'build': {'revision': '1'*40, 'arch': 'amd64'}}}, 0
        if url.endswith('/diagnostics'):
            return 200, {'incomplete': [], 'log_sink_failures': 0, 'observation_failures': 0}, 0
        if url.endswith('/consensus'):
            return 200, {'state': 'Leader' if node == 'c0' else 'Follower'}, 0
        return 200, {'expected': 5, 'collected': 5, 'alerts': []}, 0

    def test_interrupted_apply_always_attempts_revert_and_keeps_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = pathlib.Path(tmp)
            args = self.fixture(directory)
            out = directory / 'evidence'; out.mkdir()
            report = {}
            tick = iter(range(1000))
            calls = []
            def hook(argv, timeout):
                calls.append(argv[0])
                if argv[0] == 'fixture-apply':
                    raise RuntimeError('fixture interrupted after changing fault state')
                return 0
            with mock.patch.object(acceptance.ssl, 'create_default_context', return_value=mock.Mock()), \
                 mock.patch.object(acceptance, 'json_get', side_effect=self.responses), \
                 mock.patch.object(acceptance, 'get', return_value=(200, b'EXPECTED', 0)), \
                 mock.patch.object(acceptance, 'command', side_effect=hook), \
                 mock.patch.object(acceptance.time, 'monotonic', side_effect=lambda: next(tick)), \
                 mock.patch.object(acceptance.threading, 'Event', Event), \
                 mock.patch.object(acceptance.concurrent.futures, 'ThreadPoolExecutor', Pool):
                with self.assertRaises(RuntimeError):
                    acceptance.vm(args, out, report)
            self.assertEqual(calls, ['fixture-apply', 'fixture-revert'])
            self.assertTrue(report['cleanup_verified'])
            self.assertTrue((out / 'samples.json').is_file())

    def test_bad_canary_never_applies_a_fault(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = pathlib.Path(tmp)
            args = self.fixture(directory)
            out = directory / 'evidence'; out.mkdir()
            with mock.patch.object(acceptance.ssl, 'create_default_context', return_value=mock.Mock()), \
                 mock.patch.object(acceptance, 'json_get', side_effect=self.responses), \
                 mock.patch.object(acceptance, 'get', return_value=(200, b'CORRUPT', 0)), \
                 mock.patch.object(acceptance, 'command') as hook:
                with self.assertRaisesRegex(ValueError, 'baseline'):
                    acceptance.vm(args, out, {})
                hook.assert_not_called()

    def test_live_wrong_node_never_applies_a_fault(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = pathlib.Path(tmp)
            args = self.fixture(directory)
            out = directory / 'evidence'; out.mkdir()
            def wrong(opener, url, timeout):
                status, body, elapsed = self.responses(opener, url, timeout)
                if url.endswith('/compatibility'):
                    body['node_id'] = 'other'
                return status, body, elapsed
            with mock.patch.object(acceptance.ssl, 'create_default_context', return_value=mock.Mock()), \
                 mock.patch.object(acceptance, 'json_get', side_effect=wrong), \
                 mock.patch.object(acceptance, 'command') as hook:
                with self.assertRaisesRegex(ValueError, 'identity'):
                    acceptance.vm(args, out, {})
                hook.assert_not_called()


if __name__ == '__main__':
    unittest.main()
