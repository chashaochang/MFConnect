#!/usr/bin/env python3
"""Package a registry-backed Docker deployment and an optional offline bundle."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--images', type=Path, required=True)
    parser.add_argument('--server-image-id', required=True)
    parser.add_argument('--gateway-image-id', required=True)
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/release')
    args = parser.parse_args()
    if not re.fullmatch(r'\d+\.\d+\.\d+', args.version) or any(not re.fullmatch(r'sha256:[0-9a-f]{64}', v) for v in (args.server_image_id, args.gateway_image_id)):
        parser.error('invalid version or image identity')
    files = {}
    for source, target in (('compose.yaml', 'compose.yaml'), ('.env.example', '.env.example'),
                           ('deploy/docker/run.sh', 'docker-run.sh'),
                           ('README.md', 'README.md'), ('docs/deployment.md', 'docs/deployment.md'),
                           ('docs/api.md', 'docs/api.md'), ('docs/development.md', 'docs/development.md')):
        files[target] = (ROOT / source).read_bytes()
    files['config/.keep'] = b''
    metadata = {'schema_version': 2, 'release_version': args.version, 'architecture': 'linux/amd64',
                'images': {'ghcr.io/chashaochang/mfconnect-server:' + args.version + '-docker.1': args.server_image_id,
                           'ghcr.io/chashaochang/mfconnect-gateway:' + args.version: args.gateway_image_id},
                'host_requirements': {'docker': ['Docker Engine'], 'compose': ['Docker Engine', 'Docker Compose V2']},
                'default_tls': 'provided', 'optional_dns_providers': ['cloudflare', 'alidns', 'dnspod']}
    files['bundle.json'] = (json.dumps(metadata, indent=2) + '\n').encode()
    args.output.mkdir(parents=True, exist_ok=True)
    archives = {}
    for suffix in ('deploy', 'docker-amd64'):
        target = args.output / ('mfconnect-' + args.version + '-' + suffix + '.tar.gz')
        if target.exists(): parser.error('release file already exists; use a new output directory or version')
        content = dict(files)
        if suffix == 'docker-amd64': content['images.tar.gz'] = args.images.read_bytes()
        content['CONTENTS.sha256'] = ''.join(hashlib.sha256(data).hexdigest() + '  ' + name + '\n'
                                            for name,data in sorted(content.items())).encode()
        with target.open('wb') as output:
            with gzip.GzipFile(filename='', fileobj=output, mode='wb', mtime=0) as zipped:
                with tarfile.open(fileobj=zipped, mode='w') as archive:
                    for name, data in sorted(content.items()):
                        entry = tarfile.TarInfo('mfconnect/' + name)
                        entry.size, entry.mode, entry.mtime = len(data), 0o644, 0
                        archive.addfile(entry, io.BytesIO(data))
        archives[target.name] = hashlib.sha256(target.read_bytes()).hexdigest()
        print('Built ' + target.name)
    (args.output / 'SHA256SUMS').write_text(''.join(digest + '  ' + name + '\n' for name,digest in sorted(archives.items())))
    (args.output / 'bundle.json').write_text(json.dumps(metadata, indent=2) + '\n')


if __name__ == '__main__': main()
