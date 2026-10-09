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
    def package(self, directory, change=None):
        revision, version = '2' * 40, '0.4.0-rc.9'
        elf = bytearray(64); elf[:7] = b'\x7fELF\x02\x01\x01'; struct.pack_into('<H', elf, 18, 62)
        files = {p: bytes(elf) + revision.encode() if p.startswith('bin/') else b'fixture' for p in verifier.INVENTORY}
        files['manifest.json'] = json.dumps({'version': version, 'revision': revision, 'os': 'linux', 'arch': 'amd64', 'state_profile': 'titanus-state/v3'}).encode()
        files['SHA256SUMS'] = ''.join(hashlib.sha256(files[p]).hexdigest() + '  ' + p + '\n' for p in sorted(verifier.INVENTORY)).encode()
        if change:
            change(files)
        archive = directory / f'titanus-{version}-linux-amd64.tar.gz'
        with tarfile.open(archive, 'w:gz') as tar:
            for path, data in files.items():
                member = tarfile.TarInfo(archive.name[:-7] + '/' + path); member.size = len(data)
                tar.addfile(member, io.BytesIO(data))
        archive.with_name(archive.name + '.sha256').write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + '  ' + archive.name + '\n')
        return archive, revision, version

    def test_full_inventory_and_architecture(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.package(pathlib.Path(tmp))
            result = verifier.verify(*args)
            self.assertEqual(result['inventory_files_verified'], 11)
            self.assertEqual(result['elf_binaries_verified'], 4)

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


if __name__ == '__main__':
    unittest.main()
