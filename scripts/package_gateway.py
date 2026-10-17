#!/usr/bin/env python3
"""Stage only reviewed gateway files, a pinned ACME source and verified plugin."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
ACME_URL = 'https://codeload.github.com/acmesh-official/acme.sh/tar.gz/refs/tags/3.1.2'
ACME_SHA256 = 'a51511ad0e2912be45125cf189401e4ae776ca1a29d5768f020a1e35a9560186'


def stage(output, plugin, acme):
    if output.exists() and any(output.iterdir()): raise ValueError('Gateway build context must be empty')
    if hashlib.sha256(acme).hexdigest() != ACME_SHA256: raise ValueError('ACME source checksum mismatch')
    metadata = json.loads((plugin / 'release.json').read_text())
    filename = metadata['payload']
    if not re.fullmatch(r'mfconnect-[0-9.]+-linux-amd64\.tar\.gz', filename): raise ValueError('Invalid plugin filename')
    if metadata['release_base'] != 'https://downloads.example.invalid/mfconnect': raise ValueError('Use a generic plugin download template')
    payload = (plugin / filename).read_bytes()
    if hashlib.sha256(payload).hexdigest() != metadata['sha256']: raise ValueError('Plugin payload checksum mismatch')
    files = {}
    for source, target in [('deploy/gateway/Dockerfile', 'Dockerfile'), ('deploy/gateway/gateway.py', 'app/gateway.py'),
                           ('deploy/gateway/acme-wrapper.sh', 'acme-wrapper.sh'),
                           ('deploy/unified/manage.py', 'app/manage.py'), ('deploy/unified/tencent.py', 'app/tencent.py')]:
        files[target] = (ROOT / source).read_bytes()
    for name in (filename, 'mfconnect.plg', 'release.json'): files['plugin/' + name] = (plugin / name).read_bytes()
    with tarfile.open(fileobj=io.BytesIO(acme)) as archive:
        for member in archive:
            if member.isdir(): continue
            parts = Path(member.name).parts
            if not member.isfile() or not parts or parts[0] != 'acme.sh-3.1.2' or '..' in parts: raise ValueError('Invalid upstream archive entry')
            files['acme/' + '/'.join(parts[1:])] = archive.extractfile(member).read()
    for name, data in files.items():
        target = output / name; target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data); target.chmod(0o755 if name.endswith('.sh') else 0o644)
    print('Staged gateway with complete pinned ACME source and license')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plugin-dir', type=Path, required=True)
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/gateway-context')
    parser.add_argument('--acme-archive', type=Path)
    args = parser.parse_args()
    if args.acme_archive: acme = args.acme_archive.read_bytes()
    else:
        with urllib.request.urlopen(ACME_URL, timeout=60) as response: acme = response.read()
    stage(args.output, args.plugin_dir, acme)


if __name__ == '__main__': main()
