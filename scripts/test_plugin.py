#!/usr/bin/env python3
"""Exercise installer/rollback in an isolated filesystem, not on an Unraid host."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import tarfile
import xml.etree.ElementTree as ET

parser = argparse.ArgumentParser()
parser.add_argument('--package-dir', type=Path, help='Validate the actual offline .plg and payload')
parser.add_argument('--legacy-config', action='store_true', help='Also exercise the pre-gateway FRP configuration format')
args = parser.parse_args()

root = Path(__file__).resolve().parents[1]
daemon = root / 'dist/mfconnectd'
server = root / 'dist/mfconnect-server'
assert daemon.is_file() and server.is_file(), 'Run make build first'

with tempfile.TemporaryDirectory(prefix='mfconnect-plugin-test-') as directory:
    test = Path(directory)
    filesystem = test / 'filesystem'
    env = {**os.environ, 'MFCONNECT_TEST_ROOT': str(filesystem)}
    payload = test / 'payload'
    payload.mkdir()
    shutil.copytree(root / 'plugin', payload, dirs_exist_ok=True)
    shutil.copy2(daemon, payload / 'mfconnectd')
    version = subprocess.check_output([daemon, 'version'], text=True).strip()
    (payload / 'VERSION').write_text(version + '\n')
    (payload / 'FRP_VERSION').write_text('0.71.1\n')
    # Transport is not used by this installer test; actual frp is tested by e2e.py.
    (payload / 'frpc').write_text('#!/bin/sh\necho 0.71.1\n')
    (payload / 'frpc').chmod(0o755)
    if args.package_dir:
        release = json.loads((args.package_dir / 'release.json').read_text())
        version = release['version']
        assert release['local_only'], 'Package test expects an offline bundle'
        manifest = ET.parse(args.package_dir / 'mfconnect.plg').getroot()
        assert 'pluginURL' not in manifest.attrib
        inline = manifest.find('./FILE/INLINE').text
        subprocess.run(['bash', '-n'], input=inline, text=True, check=True)
        cache = filesystem / 'boot/config/plugins/mfconnect/packages' / release['payload']
        cache.parent.mkdir(parents=True)
        cache.write_bytes(b'corrupt payload')
        failed = subprocess.run(['bash', '-c', inline], env=env, capture_output=True)
        assert failed.returncode != 0, 'Corrupt cached package was accepted'
        shutil.copy2(args.package_dir / release['payload'], cache)
        with tarfile.open(cache, 'r:gz') as archive:
            assert not any('easytier' in item.name.lower() for item in archive.getmembers())
            archive.extractall(payload, filter='data')
        subprocess.run(['bash', '-c', inline], env=env, check=True, timeout=30)
    else:
        subprocess.run(['bash', payload / 'install.sh'], env=env, check=True, timeout=30)
    control = filesystem / 'etc/rc.d/rc.mfconnect'
    current = filesystem / 'usr/local/lib/mfconnect/current'
    assert current.resolve().name == version
    config = filesystem / 'boot/config/plugins/mfconnect/config.json'
    tls = test / 'tls'
    subprocess.run([server, 'init', '--dir', tls], check=True, stdout=subprocess.DEVNULL)
    subprocess.run([daemon, 'init', '--config', config, '--server', 'https://localhost:1', '--ca', tls / 'ca.crt'], check=True)
    assert json.loads(config.read_text())['transport'] == 'frp'
    if args.legacy_config:
        legacy = json.loads(config.read_text())
        del legacy['transport']
        del legacy['gateway_port']
        config.write_text(json.dumps(legacy))
    original = config.read_bytes()
    try:
        subprocess.run([control, 'start'], env=env, check=True)
        subprocess.run(['bash', payload / 'install.sh'], env=env, check=True, timeout=30)
        assert config.read_bytes() == original
        subprocess.run([control, 'status'], env=env, check=True)
        # 用户停用后的重装/开机安装必须保持停用，不作为启动失败回滚。
        subprocess.run([control, 'stop'], env=env, check=True)
        disabled = config.parent / 'disabled'
        disabled.write_text('disabled\n')
        subprocess.run(['bash', payload / 'install.sh'], env=env, check=True, timeout=30)
        assert subprocess.run([control, 'status'], env=env, capture_output=True).returncode != 0
        assert config.read_bytes() == original
        disabled.unlink()
        subprocess.run([control, 'start'], env=env, check=True)
        old = json.loads(config.read_text())
        old.update(transport='easytier', gateway_port=18743)
        config.write_text(json.dumps(old))
        incompatible = config.read_bytes()
        rejected = subprocess.run(['bash', payload / 'install.sh'], env=env, capture_output=True, timeout=30)
        assert rejected.returncode != 0 and config.read_bytes() == incompatible
        assert current.resolve().name == version
        config.write_bytes(original)
        broken = test / 'broken'
        shutil.copytree(payload, broken)
        (broken / 'VERSION').write_text('99.0.0\n')
        (broken / 'mfconnectd').write_text('#!/bin/sh\ncase "$1" in version) echo 99.0.0;; check) exit 0;; *) exit 1;; esac\n')
        (broken / 'mfconnectd').chmod(0o755)
        result = subprocess.run(['bash', broken / 'install.sh'], env=env, timeout=30)
        assert result.returncode != 0
        assert current.resolve().name == version
        assert config.read_bytes() == original
        subprocess.run([control, 'status'], env=env, check=True)
        print('PASS: plugin first install, reinstall with binding/config preserved, persistent disabled state, failed-start rollback')
        print('PASS: FRP is default; EasyTier config rejected before replacing an existing installation')
        if args.package_dir:
            print('PASS: packaged .plg installer with real binaries; checksum mismatch rejected')
    finally:
        subprocess.run([control, 'stop'], env=env, check=True)
