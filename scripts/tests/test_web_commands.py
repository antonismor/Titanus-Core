"""Sandboxed entry-point checks: no systemd or network changes."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
IDENTITY = json.dumps({'version':'0.4.0-rc.17','revision':'a'*40,'state_profile':'titanus-state/v3'})

@unittest.skipUnless(os.geteuid() == 0, 'entry points require root')
class WebCommandTests(unittest.TestCase):
    def fixture(self, folder, name, active=True):
        root=Path(folder); binaries=root/'bin';binaries.mkdir()
        (root/'access.token').write_text('a'*64+'\n')
        text=(SCRIPTS/name).read_text().replace('/var/lib/titanus/installer-web', str(root))
        script=root/name;script.write_text(text);script.chmod(0o755)
        def executable(name, text):
            target=binaries/name;target.write_text('#!/bin/sh\n'+text);target.chmod(0o755)
        executable('systemctl', 'echo "$*" >> "'+str(root/'systemctl.log')+'"\n' + ('exit 0\n' if active else '[ "$1" != is-active ]\n'))
        executable('titanus', 'if [ "$1" = version ]; then\ncat <<\'JSON\'\n'+IDENTITY+'\nJSON\nelse\nprintf "%s\\n" "$@"\nfi\n')
        executable('titanus-installer-web', 'if [ \"$1\" = --prepare-tls ]; then echo \'{\"ca_file\":\"/tmp/test-ca.crt\",\"ca_sha256\":\"test-fingerprint\"}\'; else cat <<\'JSON\'\n'+IDENTITY+'\nJSON\nfi\n')
        executable('ip', 'if [ "$3" = route ]; then exit 2; fi\ncat <<\'JSON\'\n'+json.dumps([{'addr_info':[{'local':'2001:db8::1','scope':'global'},{'local':'127.0.0.1','scope':'host'},{'local':'192.0.2.44','scope':'global'}]}])+'\nJSON\n')
        env=dict(os.environ, PATH=str(binaries)+os.pathsep+os.environ['PATH'])
        return script,env,root/'systemctl.log'

    def test_gui_detects_ipv4_without_default_route(self):
        with tempfile.TemporaryDirectory() as directory:
            script,env,_=self.fixture(directory,'tuigui')
            result=subprocess.run([str(script)],env=env,capture_output=True,text=True,timeout=20)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertIn('https://192.0.2.44:8088/#access=',result.stdout)
            self.assertNotIn('https://2001:',result.stdout)

    def test_terminal_only_forwards_setup_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            script,env,log=self.fixture(directory,'tuicli',active=False)
            result=subprocess.run([str(script),'--help'],env=env,capture_output=True,text=True,timeout=5)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertIn('setup\n--help',result.stdout)
            self.assertNotIn('stop ',log.read_text())

    def test_terminal_switch_fails_closed_when_web_api_cannot_confirm_idle(self):
        with tempfile.TemporaryDirectory() as directory:
            script,env,log=self.fixture(directory,'tuicli')
            # Invalid token path guarantees rejection without contacting any service.
            (Path(directory)/'access.token').unlink()
            result=subprocess.run([str(script)],env=env,capture_output=True,text=True,timeout=5)
            self.assertNotEqual(result.returncode,0)
            self.assertIn('No service was stopped',result.stderr)
            self.assertNotIn('stop ',log.read_text())

    def test_prepare_failure_never_starts_or_stops_service(self):
        for name in ('tuigui','tuicli'):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                script,env,log=self.fixture(directory,name)
                titanus=Path(directory)/'bin/titanus'
                titanus.write_text('#!/bin/sh\nif [ "$1" = installer ]; then echo PREPARE_FAILED >&2; exit 31; fi\nexit 0\n')
                result=subprocess.run([str(script)],env=env,capture_output=True,text=True,timeout=5)
                self.assertEqual(result.returncode,31)
                self.assertIn('PREPARE_FAILED',result.stderr)
                self.assertFalse(log.exists(), 'service touched after dependency preparation failed')

    def test_gui_reports_ca_trust_and_never_recommends_bypassing_tls(self):
        with tempfile.TemporaryDirectory() as directory:
            script,env,_=self.fixture(directory,'tuigui')
            result=subprocess.run([str(script)],env=env,capture_output=True,text=True,timeout=20)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertIn('Issuer CA SHA256 fingerprint: test-fingerprint',result.stdout)
            self.assertIn('localhost',result.stdout)
            self.assertIn('trusted roots',result.stdout)
            self.assertNotIn('confirm it',result.stdout)
