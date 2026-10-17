#!/usr/bin/env python3
"""Fetch the fixed official Linux amd64 release and its corresponding source."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile
import urllib.request
import zipfile

VERSION = '2.6.4'
# Binary digest published by GitHub's official release assets endpoint.
BINARY_SHA256 = '61b659eaedba658fa66fe47d17e1426cdd77e5d02fa15fed447bb4357c09dfd6'
# Corresponding upstream tag archive fetched from GitHub codeload over HTTPS.
SOURCE_SHA256 = '352c0866da709415a837405a6ce4f51b8dfae27e5d5c1da1fb4d8f7338e46795'
BINARY_URL = f'https://github.com/EasyTier/EasyTier/releases/download/v{VERSION}/easytier-linux-x86_64-v{VERSION}.zip'
SOURCE_URL = f'https://codeload.github.com/EasyTier/EasyTier/tar.gz/refs/tags/v{VERSION}'

def verified(path, url, digest):
    if path:
        data = path.read_bytes()
    else:
        with urllib.request.urlopen(url, timeout=60) as response:
            data = response.read()
    if hashlib.sha256(data).hexdigest() != digest:
        raise SystemExit('EasyTier archive SHA256 mismatch; no binary was installed')
    return data

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--archive', type=Path, help='Previously downloaded release ZIP; still verified')
    parser.add_argument('--source-archive', type=Path, help='Previously downloaded official source tarball; still verified')
    parser.add_argument('--output', type=Path, default=Path('dist/tools'))
    args = parser.parse_args()
    binary = verified(args.archive, BINARY_URL, BINARY_SHA256)
    source = verified(args.source_archive, SOURCE_URL, SOURCE_SHA256)
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = {'version': VERSION, 'release_url': BINARY_URL, 'release_sha256': BINARY_SHA256, 'source_url': SOURCE_URL, 'source_sha256': SOURCE_SHA256, 'binary_sha256': {}}
    with zipfile.ZipFile(io.BytesIO(binary)) as archive:
        for name in ('easytier-core', 'easytier-cli'):
            data = archive.read('easytier-linux-x86_64/' + name)
            (args.output / name).write_bytes(data)
            (args.output / name).chmod(0o755)
            manifest['binary_sha256'][name] = hashlib.sha256(data).hexdigest()
    with tarfile.open(fileobj=io.BytesIO(source), mode='r:gz') as archive:
        license_file = archive.extractfile(f'EasyTier-{VERSION}/LICENSE')
        (args.output / 'EASYTIER-LICENSE').write_bytes(license_file.read())
    (args.output / f'easytier-source-v{VERSION}.tar.gz').write_bytes(source)
    (args.output / 'EASYTIER-NOTICE').write_text(
        f'EasyTier v{VERSION}, upstream unmodified release binaries.\n'
        'Copyright: EasyTier contributors. License: LGPL-3.0 (see EASYTIER-LICENSE).\n'
        f'Corresponding upstream source included: easytier-source-v{VERSION}.tar.gz\n'
        f'Source: {SOURCE_URL}\nRelease: {BINARY_URL}\n'
        'Build instructions and dependency lockfile are included in the source archive.\n'
        'MF Connect invokes this independently replaceable executable as a child process.\n')
    (args.output / 'EASYTIER-RELEASE.json').write_text(json.dumps(manifest, indent=2) + '\n')
    print(f'Verified EasyTier {VERSION} binaries and source installed in {args.output}')

if __name__ == '__main__':
    main()
