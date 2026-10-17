#!/usr/bin/env python3
"""Check real archives exclude private notes beside otherwise valid source files."""
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class PackagingScopeTest(unittest.TestCase):
    def test_unlisted_files_are_not_published(self):
        marker = b'PRIVATE_DEPLOYMENT_FIXTURE_MUST_STAY_LOCAL'
        with tempfile.TemporaryDirectory(prefix='mfconnect-package-scope-') as directory:
            fixture = Path(directory)
            for name in ('Dockerfile', '.dockerignore', 'compose.yaml', '.env.example', 'README.md'):
                shutil.copy2(ROOT / name, fixture / name)
            for name in ('plugin', 'deploy', 'docs'):
                shutil.copytree(ROOT / name, fixture / name, ignore=shutil.ignore_patterns('__pycache__'))
            (fixture / 'scripts').mkdir()
            for name in ('package_docker.py', 'package_plugin.py'):
                shutil.copy2(ROOT / 'scripts' / name, fixture / 'scripts' / name)
            for name in ('mfconnect-server', 'mfconnectd', 'mfconnect-updater',
                         'tools/frps', 'tools/frpc', 'tools/FRP-LICENSE', 'tools/FRP_VERSION'):
                source = ROOT / 'dist' / name
                self.assertTrue(source.is_file(), f'Build binaries and FRP before this test: {name}')
                target = fixture / 'dist' / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.symlink_to(source)
            for name in ('PRIVATE_NOTES.md', 'deploy/unified/production.json',
                         'plugin/private.env', 'plugin/notes.md'):
                (fixture / name).write_bytes(marker)
            subprocess.run(['python3', fixture / 'scripts/package_docker.py',
                            '--output', fixture / 'output/cloud'], check=True, capture_output=True)
            subprocess.run(['python3', fixture / 'scripts/package_plugin.py', '--local',
                            '--binary', fixture / 'dist/mfconnectd',
                            '--frpc', fixture / 'dist/tools/frpc',
                            '--frp-license', fixture / 'dist/tools/FRP-LICENSE',
                            '--output', fixture / 'output/plugin'], check=True, capture_output=True)
            archives = list((fixture / 'output').rglob('*.tar.gz'))
            self.assertEqual(len(archives), 2)
            for path in archives:
                with tarfile.open(path) as archive:
                    self.assertTrue(archive.getmembers())
                    for item in archive.getmembers():
                        if item.isfile():
                            self.assertNotIn(marker, archive.extractfile(item).read(), item.name)


if __name__ == '__main__':
    unittest.main()
