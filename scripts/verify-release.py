#!/usr/bin/env python3
"""Inspect both native packages without extracting or executing their binaries."""
import argparse
import hashlib
import json
import pathlib
import re
import struct
import tarfile

INVENTORY = {
    'bin/titanus', 'bin/titanusd', 'bin/titanus-agent', 'bin/titanus-init',
    'systemd/titanusd.service', 'systemd/titanus-agent.service',
    'scripts/install-release.py', 'scripts/install-release.sh',
    'docs/RELEASE.md', 'LICENSE', 'manifest.json',
}


def verify(archive, revision, version):
    archive = pathlib.Path(archive)
    if archive.stat().st_size > 64 << 20 or archive.with_name(archive.name + '.sha256').stat().st_size > 4096:
        raise ValueError('archive/checksum file bounds exceeded')
    digest, name = archive.with_name(archive.name + '.sha256').read_text().strip().split('  ')
    actual = hashlib.sha256(archive.read_bytes()).hexdigest()
    if not re.fullmatch('[0-9a-f]{64}', digest) or name != archive.name or actual != digest:
        raise ValueError('outer archive checksum/name mismatch')
    with tarfile.open(archive, 'r:gz') as tar:
        members = tar.getmembers()
        if len(members) > 64 or sum(m.size for m in members) > 128 << 20:
            raise ValueError('archive bounds exceeded')
        files, roots = {}, set()
        for member in members:
            parts = pathlib.PurePosixPath(member.name).parts
            if not parts or member.name.startswith('/') or '..' in parts or member.issym() or member.islnk():
                raise ValueError('unsafe archive member')
            roots.add(parts[0])
            if member.isdir():
                continue
            if not member.isfile() or len(parts) < 2:
                raise ValueError('unsupported archive member')
            relative = '/'.join(parts[1:])
            if relative in files:
                raise ValueError('duplicate member')
            files[relative] = tar.extractfile(member).read()
        if len(roots) != 1 or set(files) != INVENTORY | {'SHA256SUMS'}:
            raise ValueError('incomplete or unexpected package inventory')
        inventory = {}
        for line in files['SHA256SUMS'].decode().splitlines():
            sha, path = line.split('  ')
            if path in inventory or path not in INVENTORY or not re.fullmatch('[0-9a-f]{64}', sha):
                raise ValueError('invalid internal checksum inventory')
            inventory[path] = sha
        if set(inventory) != INVENTORY:
            raise ValueError('missing internal checksum')
        for path, sha in inventory.items():
            if hashlib.sha256(files[path]).hexdigest() != sha:
                raise ValueError('internal checksum mismatch: ' + path)
        meta = json.loads(files['manifest.json'])
        if meta != {'version': version, 'revision': revision, 'os': 'linux',
                    'arch': meta.get('arch'), 'state_profile': 'titanus-state/v3'}:
            raise ValueError('manifest identity mismatch')
        arch = meta['arch']
        if arch not in ('amd64', 'arm64'):
            raise ValueError('unsupported architecture')
        stem = f'titanus-{version}-linux-{arch}'
        if roots != {stem} or archive.name != stem + '.tar.gz':
            raise ValueError('archive root/name identity mismatch')
        for path in sorted(INVENTORY):
            if not path.startswith('bin/'):
                continue
            data = files[path]
            if len(data) < 64 or data[:7] != b'\x7fELF\x02\x01\x01':
                raise ValueError('not a 64-bit little-endian ELF: ' + path)
            if struct.unpack_from('<H', data, 18)[0] != {'amd64': 62, 'arm64': 183}[arch]:
                raise ValueError('ELF architecture mismatch: ' + path)
            if revision.encode() not in data:
                raise ValueError('exact compiled revision absent: ' + path)
        return {'archive': archive.name, 'sha256': actual, 'manifest': meta,
                'inventory_files_verified': len(inventory), 'elf_binaries_verified': 4}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--revision', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('archives', nargs=2)
    args = parser.parse_args()
    if not re.fullmatch('[0-9a-f]{40}', args.revision):
        parser.error('exact 40-character revision required')
    records = [verify(p, args.revision, args.version) for p in args.archives]
    if {r['manifest']['arch'] for r in records} != {'amd64', 'arm64'}:
        raise ValueError('both distinct native architectures required')
    print(json.dumps({'format': 'titanus-release-verification/v1', 'packages': records}, sort_keys=True))


if __name__ == '__main__':
    main()
