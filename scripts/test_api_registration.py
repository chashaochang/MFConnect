#!/usr/bin/env python3
"""Verify reversible API module registration in a disposable filesystem."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
node = shutil.which('node') or '/usr/bin/node'
with tempfile.TemporaryDirectory(prefix='mfconnect-api-registration-') as directory:
    fs = Path(directory)
    api = fs / 'usr/local/unraid-api'
    (api / 'node_modules/@nestjs/core').mkdir(parents=True)
    pkg = api / 'package.json'
    pkg.write_text(json.dumps({'name': '@unraid/api', 'dependencies': {'existing': '1.0.0'}}))
    cfg = fs / 'boot/config/plugins/dynamix.my.servers/configs/api.json'
    cfg.parent.mkdir(parents=True)
    cfg.write_text(json.dumps({'plugins': ['existing-plugin'], 'untouched': {'nested': True}}))
    original_pkg, original_cfg = json.loads(pkg.read_text()), json.loads(cfg.read_text())
    source = fs / 'usr/local/lib/mfconnect/current/api'
    source.parent.mkdir(parents=True)
    shutil.copytree(root / 'plugin/api', source)
    env = {**os.environ, 'MFCONNECT_TEST_ROOT': str(fs)}
    command = [node, str(root / 'plugin/register-api.js')]
    for _ in range(2):
        subprocess.run(command + ['install'], env=env, check=True)
    subprocess.run(command + ['check'], env=env, check=True)
    result = json.loads(cfg.read_text())
    assert result['plugins'] == ['existing-plugin', 'unraid-api-plugin-mfconnect']
    assert result['untouched'] == original_cfg['untouched']
    subprocess.run(command + ['remove'], env=env, check=True)
    subprocess.run(command + ['remove'], env=env, check=True)
    assert json.loads(pkg.read_text()) == original_pkg
    assert json.loads(cfg.read_text()) == original_cfg
    assert not (api / 'node_modules/unraid-api-plugin-mfconnect').exists()
    print('PASS: repeat install, check, scoped uninstall and unrelated API configuration preserved')
