#!/usr/bin/env python3
"""Publish standalone Unraid installers without changing released payloads."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import xml.etree.ElementTree as ET

REPOSITORY = 'chashaochang/MFConnect'
PLACEHOLDER = 'https://downloads.example.invalid/mfconnect'
START = '<!-- mfconnect-installation:start -->'
END = '<!-- mfconnect-installation:end -->'


def checked_version(version):
    if not re.fullmatch(r'\d+\.\d+\.\d+', version):
        raise ValueError('Invalid release version')
    return version


def package_plugin(source, output, version):
    """Rehost the gateway's verified payload; only its download URLs change."""
    checked_version(version)
    metadata = json.loads((source / 'release.json').read_text())
    name = 'mfconnect-' + version + '-linux-amd64.tar.gz'
    if (metadata.get('version') != version or metadata.get('payload') != name
            or metadata.get('local_only') is not False
            or metadata.get('release_base') != PLACEHOLDER):
        raise ValueError('Expected the generic plugin from the matching gateway image')
    payload = (source / name).read_bytes()
    digest = hashlib.sha256(payload).hexdigest()
    if metadata.get('sha256') != digest:
        raise ValueError('Plugin payload checksum mismatch')
    with tarfile.open(source / name, 'r:gz') as archive:
        if archive.extractfile('VERSION').read().decode().strip() != version:
            raise ValueError('Plugin payload version mismatch')
    manifest = (source / 'mfconnect.plg').read_text()
    plugin = ET.fromstring(manifest)
    if (plugin.tag != 'PLUGIN' or plugin.get('name') != 'mfconnect'
            or plugin.get('version') != version
            or plugin.get('pluginURL') != PLACEHOLDER + '/mfconnect.plg'):
        raise ValueError('Plugin manifest identity mismatch')
    inline = plugin.find('./FILE/INLINE').text
    if digest not in inline or PLACEHOLDER + '/' + name not in inline:
        raise ValueError('Plugin manifest does not pin the expected payload')
    base = 'https://github.com/' + REPOSITORY + '/releases/download/v' + version
    update_url = 'https://github.com/' + REPOSITORY + '/releases/latest/download/mfconnect.plg'
    manifest = manifest.replace('pluginURL="' + PLACEHOLDER + '/mfconnect.plg"',
                                'pluginURL="' + update_url + '"').replace(PLACEHOLDER, base)
    rewritten = ET.fromstring(manifest)
    if rewritten.get('pluginURL') != update_url:
        raise ValueError('Plugin update URL was not rewritten')
    subprocess.run(['bash', '-n'], input=rewritten.find('./FILE/INLINE').text,
                   text=True, check=True)
    metadata.update(release_base=base, update_url=update_url)
    files = {name: payload, 'mfconnect.plg': manifest.encode(),
             'plugin-release.json': (json.dumps(metadata, indent=2) + '\n').encode()}
    files['PLUGIN-SHA256SUMS'] = ''.join(
        hashlib.sha256(data).hexdigest() + '  ' + filename + '\n'
        for filename, data in sorted(files.items())).encode()
    output.mkdir(parents=True, exist_ok=True)
    # Never silently overwrite another package while preparing an upload.
    for filename, data in files.items():
        target = output / filename
        if target.exists() and target.read_bytes() != data:
            raise ValueError('Refusing to replace a different asset: ' + filename)
    for filename, data in files.items():
        (output / filename).write_bytes(data)
    return sorted(files)


def installation_notes(version):
    checked_version(version)
    project = 'https://github.com/' + REPOSITORY
    assets = project + '/releases/download/v' + version
    return f'''{START}
## 云服务器：部署中继

支持普通 Docker 和 Docker Compose，任选一种。容器镜像：

- `ghcr.io/chashaochang/mfconnect-server:{version}-docker.1`
- `ghcr.io/chashaochang/mfconnect-gateway:{version}`

| 下载文件 | 用途 |
| --- | --- |
| [mfconnect-{version}-deploy.tar.gz]({assets}/mfconnect-{version}-deploy.tar.gz) | 服务器部署配置，启动时在线拉取镜像 |
| [mfconnect-{version}-docker-amd64.tar.gz]({assets}/mfconnect-{version}-docker-amd64.tar.gz) | 服务器离线部署包，包含 Docker 镜像 |
| `SHA256SUMS` / `bundle.json` | 服务器包校验值 / 镜像与版本信息 |

安装步骤见 [README]({project}/blob/master/README.md#自己部署中继)。需要 Linux amd64、Docker、域名和自行配置的 SSL 证书。

## Unraid：安装插件

在 Unraid 7+ 打开 **Plugins → Install Plugin**，粘贴以下地址安装本版本：

```text
{assets}/mfconnect.plg
```

安装后打开 **Settings → MF Connect**，填写自己的中继 HTTPS 入口。

| 下载文件 | 用途 |
| --- | --- |
| `mfconnect.plg` | Unraid 插件安装清单；安装时自动下载下方程序包 |
| `mfconnect-{version}-linux-amd64.tar.gz` | Unraid 插件程序包，由安装清单自动下载，无需手动解压 |
| `PLUGIN-SHA256SUMS` / `plugin-release.json` | 插件文件校验值 / 插件版本信息 |

GitHub 安装的插件通过 Unraid 插件管理器检查后续正式版更新。已有 `https://你的入口/downloads/mfconnect/mfconnect.plg` 安装方式继续可用，跟随部署者提供的插件版本；App 内更新仍使用中继配置的签名发行频道。

此 Release 不包含 RaidMate 手机客户端。GitHub 自动附带的 **Source code** 是源码，不是安装包。
{END}'''


def merge_notes(body, version):
    section = installation_notes(version)
    if START in body or END in body:
        if body.count(START) != 1 or body.count(END) != 1 or body.index(END) < body.index(START):
            raise ValueError('Malformed managed release notes section')
        before, remainder = body.split(START, 1)
        _, after = remainder.split(END, 1)
        return before + section + after
    return body.rstrip() + ('\n\n' if body.strip() else '') + section + '\n'


def gh(*args):
    return subprocess.check_output(['gh', *args], text=True)


def verify_assets(assets, output, download):
    """Check every existing file before uploading any missing files."""
    missing = []
    for asset in assets:
        local = output / asset
        existing = download(asset)
        if existing is None:
            missing.append(local)
        elif hashlib.sha256(existing).digest() != hashlib.sha256(local.read_bytes()).digest():
            raise ValueError('Refusing to replace a different published asset: ' + asset)
    return missing


def sync_release(tag=None):
    """Fill missing plugin assets from the original, already published image."""
    selector = ['tags', tag] if tag else ['latest']
    release = json.loads(gh('api', 'repos/' + REPOSITORY + '/releases/' + '/'.join(selector)))
    if release['draft'] or release['prerelease']:
        raise ValueError('Only a published stable release can be synchronized')
    tag = release['tag_name']
    version = checked_version(tag.removeprefix('v'))
    if tag != 'v' + version:
        raise ValueError('Unexpected release tag')
    names = {asset['name'] for asset in release['assets']}
    with tempfile.TemporaryDirectory(prefix='mfconnect-release-assets-') as directory:
        work = Path(directory)
        original = work / 'original'
        original.mkdir()

        def download(name):
            if name not in names:
                return None
            gh('release', 'download', tag, '--repo', REPOSITORY, '--pattern', name,
               '--dir', str(original))
            return (original / name).read_bytes()

        bundle = json.loads(download('bundle.json'))
        image = 'ghcr.io/chashaochang/mfconnect-gateway:' + version
        identity = bundle['images'][image]
        if (bundle['release_version'] != version
                or not re.fullmatch(r'sha256:[0-9a-f]{64}', identity)):
            raise ValueError('Invalid published image identity')
        subprocess.run(['docker', 'pull', '--platform', 'linux/amd64', image], check=True)
        actual = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], text=True).strip()
        if actual != identity:
            raise ValueError('Published gateway image does not match release metadata')
        container = subprocess.check_output(['docker', 'create', '--platform', 'linux/amd64', identity], text=True).strip()
        source = work / 'plugin'
        try:
            subprocess.run(['docker', 'cp', container + ':/opt/mfconnect/plugin', str(source)], check=True)
        finally:
            subprocess.run(['docker', 'rm', container], check=True)
        output = work / 'assets'
        assets = package_plugin(source, output, version)
        missing = verify_assets(assets, output, download)
        if missing:
            gh('release', 'upload', tag, '--repo', REPOSITORY, *map(str, missing))
        notes = work / 'release-notes.md'
        updated = merge_notes(release['body'] or '', version)
        notes.write_text(updated)
        if updated != release['body']:
            gh('release', 'edit', tag, '--repo', REPOSITORY,
               '--title', 'MF Connect ' + tag + ' · 服务器 / Unraid 插件', '--notes-file', str(notes))
        print('Verified standalone plugin assets and installation instructions for ' + tag)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    package = commands.add_parser('package')
    package.add_argument('--plugin-dir', type=Path, required=True)
    package.add_argument('--output', type=Path, required=True)
    package.add_argument('--version', required=True)
    sync = commands.add_parser('sync')
    sync.add_argument('--tag', help='Default: latest published stable release')
    args = parser.parse_args()
    if args.command == 'sync':
        sync_release(args.tag)
    else:
        for name in package_plugin(args.plugin_dir, args.output, args.version):
            print('Prepared ' + name)
        (args.output / 'release-notes.md').write_text(merge_notes('', args.version))


if __name__ == '__main__':
    main()
