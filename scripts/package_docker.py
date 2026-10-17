#!/usr/bin/env python3
"""Package the Compose build context, without local credentials or state."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
VERSION = '0.7.0'
IMAGE_TAG = VERSION + '-docker.1'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, default=ROOT / 'dist/mfconnect-server')
    parser.add_argument('--frp-dir', type=Path, default=ROOT / 'dist/tools')
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/docker' / VERSION)
    args = parser.parse_args()
    files = {}
    for name in ('Dockerfile', '.dockerignore', 'compose.yaml', '.env.example',
                 'deploy/docker/entrypoint.sh', 'deploy/docker/healthcheck.sh',
                 'deploy/docker/install-updater.sh', 'deploy/docker/mfconnect-updater.service'):
        files[name] = ((ROOT / name).read_bytes(), 0o755 if name.endswith('.sh') else 0o644)
    for name in ('deploy/unified/config.example.json', 'deploy/unified/manage.py',
                 'deploy/unified/tencent.py', 'deploy/unified/mfconnect-tls.service',
                 'deploy/unified/mfconnect-tls.timer'):
        files[name] = ((ROOT / name).read_bytes(), 0o644)
    # Only reviewed user documentation belongs in a public build context.
    # Never sweep local notes or deployment reports into release archives.
    for name in ('README.md', 'docs/api.md', 'docs/deployment.md', 'docs/development.md'):
        files[name] = ((ROOT / name).read_bytes(), 0o644)
    for name, path in (('dist/mfconnect-server', args.binary),
                       ('dist/mfconnect-updater', ROOT / 'dist/mfconnect-updater'),
                       ('dist/tools/frps', args.frp_dir / 'frps')):
        data = path.read_bytes()
        if data[:6] != b'\x7fELF\x02\x01' or data[18:20] != b'\x3e\x00':
            parser.error(f'{path} is not a Linux amd64 ELF executable')
        files[name] = (data, 0o755)
    if subprocess.check_output([str(args.binary.resolve()), 'version'], text=True).strip() != VERSION:
        parser.error('server version does not match the deployment bundle')
    try:
        capabilities = json.loads(subprocess.check_output([str(args.binary.resolve()), 'capabilities'], text=True))
        if 2 not in capabilities.get('endpoint_contracts', []):
            parser.error('server must implement endpoint contract 2 for this deployment configuration')
    except (subprocess.CalledProcessError, ValueError):
        parser.error('server does not provide endpoint capabilities; rebuild before packaging')
    if subprocess.check_output([str((args.frp_dir / 'frps').resolve()), '--version'], text=True).strip() != '0.71.1':
        parser.error('FRP version does not match the server')
    for name in ('FRP-LICENSE', 'FRP_VERSION'):
        files['dist/tools/' + name] = ((args.frp_dir / name).read_bytes(), 0o644)
    checksums = ''.join(f'{hashlib.sha256(data).hexdigest()}  {name}\n' for name, (data, _) in sorted(files.items()))
    files['CONTENTS.sha256'] = (checksums.encode(), 0o644)
    prefix = 'mfconnect-' + IMAGE_TAG
    args.output.mkdir(parents=True, exist_ok=True)
    archive_path = args.output / (prefix + '-linux-amd64.tar.gz')
    with archive_path.open('wb') as output:
        with gzip.GzipFile(filename='', fileobj=output, mode='wb', mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode='w') as archive:
                for name, (data, mode) in sorted(files.items()):
                    info = tarfile.TarInfo(prefix + '/' + name)
                    info.size, info.mode, info.mtime = len(data), mode, 0
                    archive.addfile(info, io.BytesIO(data))
    digest = hashlib.sha256(archive_path.read_bytes()).hexdigest()
    (args.output / 'SHA256SUMS').write_text(f'{digest}  {archive_path.name}\n')
    (args.output / 'release.json').write_text(json.dumps({
        'server_version': VERSION, 'image_tag': IMAGE_TAG, 'transport': 'frp', 'frp_version': '0.71.1',
        'architecture': 'linux/amd64', 'artifact': archive_path.name,
        'artifact_kind': 'docker-compose-build-context', 'prebuilt_image': False,
        'sha256': digest,
    }, indent=2) + '\n')
    print(f'Built {archive_path} (Compose build context, not a prebuilt Docker image)')


if __name__ == '__main__':
    main()
