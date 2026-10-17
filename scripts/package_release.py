#!/usr/bin/env python3
"""Build a precompiled cloud-installer bundle from an explicitly reviewed file list."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
ACME_URL = 'https://codeload.github.com/acmesh-official/acme.sh/tar.gz/refs/tags/3.1.2'
ACME_SHA256 = 'a51511ad0e2912be45125cf189401e4ae776ca1a29d5768f020a1e35a9560186'
RUNTIME_FILES = (
    'compose.yaml', '.env.example',
    'deploy/docker/install-updater.sh', 'deploy/docker/mfconnect-updater.service',
    'deploy/unified/manage.py', 'deploy/unified/tencent.py',
    'deploy/unified/config.example.json',
    'deploy/unified/mfconnect-tls.service', 'deploy/unified/mfconnect-tls.timer',
    'dist/mfconnect-updater',
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--image', type=Path, required=True)
    parser.add_argument('--image-id', required=True)
    parser.add_argument('--plugin-dir', type=Path, required=True)
    parser.add_argument('--acme-archive', type=Path)
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/release')
    args = parser.parse_args()
    import re
    if not re.fullmatch(r'\d+\.\d+\.\d+', args.version) or not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image_id):
        parser.error('invalid version/image identity')
    server = subprocess.check_output([str(ROOT / 'dist/mfconnect-server'), 'version'], text=True).strip()
    plugin = json.loads((args.plugin_dir / 'release.json').read_text())
    if plugin['release_base'] != 'https://downloads.example.invalid/mfconnect':
        parser.error('installer plugin must use the documented placeholder, never a live deployment')
    if not re.fullmatch(r'mfconnect-[0-9.]+-linux-amd64\.tar\.gz', plugin['payload']):
        parser.error('invalid plugin payload name')
    if hashlib.sha256((args.plugin_dir / plugin['payload']).read_bytes()).hexdigest() != plugin['sha256']:
        parser.error('plugin payload checksum mismatch')
    files = {}
    for name in RUNTIME_FILES:
        files['runtime/' + name] = ((ROOT / name).read_bytes(), 0o755 if name.endswith('.sh') or name == 'dist/mfconnect-updater' else 0o644)
    for name in ('manage.py', 'tencent.py'):
        files['deploy/unified/' + name] = ((ROOT / 'deploy/unified' / name).read_bytes(), 0o644)
    files['deploy/install/cloud.py'] = ((ROOT / 'deploy/install/cloud.py').read_bytes(), 0o644)
    files['README.md'] = ((ROOT / 'README.md').read_bytes(), 0o644)
    files['docs/deployment.md'] = ((ROOT / 'docs/deployment.md').read_bytes(), 0o644)
    files['server-image.tar.gz'] = (args.image.read_bytes(), 0o644)
    files['plugin/' + plugin['payload']] = ((args.plugin_dir / plugin['payload']).read_bytes(), 0o644)
    files['plugin/mfconnect.plg'] = ((args.plugin_dir / 'mfconnect.plg').read_bytes(), 0o644)
    if args.acme_archive:
        acme = args.acme_archive.read_bytes()
    else:
        with urllib.request.urlopen(ACME_URL, timeout=60) as response:
            acme = response.read()
    if hashlib.sha256(acme).hexdigest() != ACME_SHA256:
        parser.error('acme.sh archive checksum mismatch')
    with tarfile.open(fileobj=io.BytesIO(acme)) as archive:
        for member in archive:
            if member.isdir():
                continue
            parts = Path(member.name).parts
            if not member.isfile() or parts[0] != 'acme.sh-3.1.2' or '..' in parts:
                parser.error('unexpected acme.sh archive entry')
            # Preserve the complete original script source and its GPL license.
            name = 'third_party/acme.sh/' + '/'.join(parts[1:])
            files[name] = (archive.extractfile(member).read(), 0o755 if name.endswith('.sh') else 0o644)
    metadata = {'schema_version': 1, 'release_version': args.version, 'server_version': server,
                'plugin_version': plugin['version'], 'plugin_payload': plugin['payload'],
                'image': 'mfconnect-server:' + server + '-docker.1', 'image_id': args.image_id}
    files['bundle.json'] = ((json.dumps(metadata, indent=2) + '\n').encode(), 0o644)
    args.output.mkdir(parents=True, exist_ok=True)
    target = args.output / ('mfconnect-' + args.version + '-linux-amd64-deploy.tar.gz')
    with target.open('wb') as output:
        with gzip.GzipFile(filename='', fileobj=output, mode='wb', mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode='w') as archive:
                for name, (data, mode) in sorted(files.items()):
                    entry = tarfile.TarInfo(name)
                    entry.size, entry.mode, entry.mtime = len(data), mode, 0
                    archive.addfile(entry, io.BytesIO(data))
    digest = hashlib.sha256(target.read_bytes()).hexdigest()
    (args.output / 'SHA256SUMS').write_text(digest + '  ' + target.name + '\n')
    (args.output / 'bundle.json').write_text(json.dumps(metadata, indent=2) + '\n')
    print('Built ' + str(target))


if __name__ == '__main__':
    main()
