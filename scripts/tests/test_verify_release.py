import hashlib
import importlib.util
import io
import json
import pathlib
import struct
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('verifier', pathlib.Path(__file__).resolve().parents[1] / 'verify-release.py')
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


class ReleaseIntegrity(unittest.TestCase):
    def package(self, directory, change=None, member_change=None):
        revision, version = '2' * 40, '0.4.0-rc.10'
        elf = bytearray(64); elf[:7] = b'\x7fELF\x02\x01\x01'; struct.pack_into('<H', elf, 18, 62)
        files = {p: bytes(elf) + revision.encode() if p.startswith('bin/') else b'fixture' for p in verifier.INVENTORY}
        files['manifest.json'] = json.dumps({'version': version, 'revision': revision, 'os': 'linux', 'arch': 'amd64', 'state_profile': 'titanus-state/v3'}).encode()
        files['SHA256SUMS'] = ''.join(hashlib.sha256(files[p]).hexdigest() + '  ' + p + '\n' for p in sorted(verifier.INVENTORY)).encode()
        if change:
            change(files)
        archive = directory / f'titanus-{version}-linux-amd64.tar.gz'
        with tarfile.open(archive, 'w:gz') as tar:
            root = archive.name[:-7]
            for directory_path in (root, *(root + '/' + p for p in ('bin', 'docs', 'scripts', 'systemd'))):
                member = tarfile.TarInfo(directory_path)
                member.type = tarfile.DIRTYPE
                member.mode = 0o755
                member.uname = member.gname = 'root'
                if member_change:
                    member_change(member)
                tar.addfile(member)
            for path, data in files.items():
                member = tarfile.TarInfo(root + '/' + path)
                member.size = len(data)
                member.mode = 0o755 if path.startswith('bin/') or path.endswith('.sh') or path in verifier.WEB_COMMANDS else 0o644
                member.uname = member.gname = 'root'
                if member_change:
                    member_change(member)
                tar.addfile(member, io.BytesIO(data))
        archive.with_name(archive.name + '.sha256').write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + '  ' + archive.name + '\n')
        return archive, revision, version

    def test_full_inventory_and_architecture(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp))
            result = verifier.verify(*args)
            self.assertEqual(result['inventory_files_verified'], 11)
            self.assertEqual(result['elf_binaries_verified'], 4)

    @staticmethod
    def mutate_member(suffix, attribute, value):
        def mutate(member):
            if member.name.endswith(suffix):
                setattr(member, attribute, value)
        return mutate

    def test_reject_world_writable_binary_with_valid_hashes(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), member_change=self.mutate_member('/bin/titanusd', 'mode', 0o777))
            with self.assertRaisesRegex(ValueError, 'permissions'):
                verifier.verify(*args)

    def test_reject_setuid_binary_with_valid_hashes(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), member_change=self.mutate_member('/bin/titanus', 'mode', 0o4755))
            with self.assertRaisesRegex(ValueError, 'permissions'):
                verifier.verify(*args)

    def test_reject_foreign_archive_owner(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), member_change=self.mutate_member('/bin/titanus', 'uid', 1000))
            with self.assertRaisesRegex(ValueError, 'owner'):
                verifier.verify(*args)

    def test_reject_permissive_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), member_change=self.mutate_member('/scripts', 'mode', 0o777))
            with self.assertRaisesRegex(ValueError, 'permissions'):
                verifier.verify(*args)

    def test_payload_corruption_despite_matching_outer_hash(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), lambda f: f.update({'bin/titanusd': b'corrupt'}))
            with self.assertRaisesRegex(ValueError, 'internal checksum'):
                verifier.verify(*args)

    def test_undeclared_member_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), lambda f: f.update({'extra-private-key': b'fixture'}))
            with self.assertRaisesRegex(ValueError, 'inventory'):
                verifier.verify(*args)

    def test_wrong_architecture_despite_valid_checksums(self):
        def wrong(files):
            data = bytearray(files['bin/titanus-init']); struct.pack_into('<H', data, 18, 183)
            files['bin/titanus-init'] = bytes(data)
            files['SHA256SUMS'] = ''.join(hashlib.sha256(files[p]).hexdigest() + '  ' + p + '\n' for p in sorted(verifier.INVENTORY)).encode()
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), wrong)
            with self.assertRaisesRegex(ValueError, 'architecture'):
                verifier.verify(*args)

    def test_hashed_maintenance_helper_and_legacy_inventory(self):
        def helper(files):
            files['scripts/maintenance_transaction.py'] = b'maintenance fixture'
            path = 'scripts/maintenance_transaction.py'
            files['SHA256SUMS'] += (hashlib.sha256(files[path]).hexdigest() + '  ' + path + '\n').encode()
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), change=helper)
            self.assertEqual(verifier.verify(*args)['inventory_files_verified'], 12)
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp))
            self.assertEqual(verifier.verify(*args)['inventory_files_verified'], 11)

    @staticmethod
    def add_web(files):
        for path in verifier.WEB_INVENTORY:
            files[path] = files['bin/titanus'] if path.startswith('bin/') else b'web fixture'
        files['SHA256SUMS'] = ''.join(hashlib.sha256(data).hexdigest() + '  ' + path + '\n'
                                      for path, data in sorted(files.items()) if path != 'SHA256SUMS').encode()

    def test_complete_web_group_and_fifth_native_binary(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), change=self.add_web)
            result = verifier.verify(*args)
            self.assertEqual(result['inventory_files_verified'], 16)
            self.assertEqual(result['elf_binaries_verified'], 5)

    def test_partial_web_group_is_rejected(self):
        def partial(files):
            self.add_web(files)
            del files['docs/WEB_INSTALLER.md']
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), change=partial)
            with self.assertRaisesRegex(ValueError, 'incomplete optional Web Installer'):
                verifier.verify(*args)

    def test_web_binary_wrong_architecture_is_rejected(self):
        def wrong(files):
            self.add_web(files)
            data = bytearray(files['bin/titanus-installer-web']); struct.pack_into('<H', data, 18, 183)
            files['bin/titanus-installer-web'] = bytes(data)
            files['SHA256SUMS'] = ''.join(hashlib.sha256(data).hexdigest() + '  ' + path + '\n'
                                          for path, data in sorted(files.items()) if path != 'SHA256SUMS').encode()
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), change=wrong)
            with self.assertRaisesRegex(ValueError, 'architecture'):
                verifier.verify(*args)

    def test_web_commands_must_be_executable(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp), change=self.add_web,
                                member_change=self.mutate_member('/scripts/tuigui', 'mode', 0o644))
            with self.assertRaisesRegex(ValueError, 'permissions'):
                verifier.verify(*args)


if __name__ == '__main__':
    unittest.main()
