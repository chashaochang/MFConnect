#!/usr/bin/env python3
"""Check immutable plugin publication and the user-facing install contract."""
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import xml.etree.ElementTree as ET

from release_assets import (END, PLACEHOLDER, REPOSITORY, START, merge_notes,
                            package_plugin, verify_assets)


class ReleaseAssetsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.work = Path(self.directory.name)
        self.source, self.output = self.work / 'plugin', self.work / 'output'
        self.source.mkdir()
        self.version = '0.8.1'
        self.name = 'mfconnect-' + self.version + '-linux-amd64.tar.gz'
        with tarfile.open(self.source / self.name, 'w:gz') as archive:
            data = (self.version + '\n').encode()
            member = tarfile.TarInfo('VERSION')
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        self.digest = hashlib.sha256((self.source / self.name).read_bytes()).hexdigest()
        self.metadata = {'version': self.version, 'connector_version': self.version,
                         'frp_version': '0.71.1', 'transport': 'frp', 'payload': self.name,
                         'sha256': self.digest, 'local_only': False, 'release_base': PLACEHOLDER}
        self.write_metadata()
        (self.source / 'mfconnect.plg').write_text(f'''<?xml version="1.0"?>
<PLUGIN name="mfconnect" version="{self.version}" pluginURL="{PLACEHOLDER}/mfconnect.plg">
<FILE Run="/bin/bash"><INLINE><![CDATA[set -e
curl --fail --location '{PLACEHOLDER}/{self.name}' -o payload
printf '%s  %s\\n' '{self.digest}' payload | sha256sum -c -
]]></INLINE></FILE></PLUGIN>''')

    def write_metadata(self):
        (self.source / 'release.json').write_text(json.dumps(self.metadata))

    def package(self):
        return package_plugin(self.source, self.output, self.version)

    def test_download_is_pinned_updates_follow_latest_and_payload_is_unchanged(self):
        names = self.package()
        original = (self.source / self.name).read_bytes()
        self.assertEqual((self.output / self.name).read_bytes(), original)
        manifest = ET.parse(self.output / 'mfconnect.plg').getroot()
        self.assertEqual(manifest.get('pluginURL'),
                         'https://github.com/' + REPOSITORY + '/releases/latest/download/mfconnect.plg')
        inline = manifest.find('./FILE/INLINE').text
        self.assertIn('/releases/download/v0.8.1/' + self.name, inline)
        self.assertIn(self.digest, inline)
        self.assertNotIn(PLACEHOLDER, (self.output / 'mfconnect.plg').read_text())
        for line in (self.output / 'PLUGIN-SHA256SUMS').read_text().splitlines():
            digest, filename = line.split('  ')
            self.assertIn(filename, names)
            self.assertEqual(digest, hashlib.sha256((self.output / filename).read_bytes()).hexdigest())
        self.assertEqual(self.package(), names)

    def test_corrupt_payload_rejected_before_output(self):
        with (self.source / self.name).open('ab') as output:
            output.write(b'corrupt')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            self.package()
        self.assertFalse(self.output.exists())

    def test_version_or_non_generic_source_rejected(self):
        for field, value in [('version', '9.9.9'), ('payload', '../escape.tar.gz'),
                             ('release_base', 'https://example.com/private'), ('local_only', True)]:
            with self.subTest(field=field):
                previous = self.metadata[field]
                self.metadata[field] = value
                self.write_metadata()
                with self.assertRaises(ValueError):
                    self.package()
                self.metadata[field] = previous

    def test_manifest_cannot_substitute_another_payload(self):
        path = self.source / 'mfconnect.plg'
        path.write_text(path.read_text().replace(self.digest, '0' * 64))
        with self.assertRaisesRegex(ValueError, 'pin the expected payload'):
            self.package()

    def test_existing_local_assets_never_overwritten(self):
        self.package()
        existing = self.output / 'mfconnect.plg'
        existing.write_text('existing different asset')
        with self.assertRaisesRegex(ValueError, 'Refusing to replace'):
            self.package()
        self.assertEqual(existing.read_text(), 'existing different asset')

    def test_remote_assets_checked_before_missing_uploads(self):
        names = self.package()
        downloads = {name: (self.output / name).read_bytes() for name in names}
        self.assertEqual(verify_assets(names, self.output, downloads.get), [])
        del downloads[self.name]
        self.assertEqual(verify_assets(names, self.output, downloads.get), [self.output / self.name])
        downloads['mfconnect.plg'] = b'different remote content'
        with self.assertRaisesRegex(ValueError, 'published asset'):
            verify_assets(names, self.output, downloads.get)

    def test_notes_preserve_release_changes_and_are_idempotent(self):
        original = '修复旧会话契约，现有插件无需升级。'
        notes = merge_notes(original, self.version)
        self.assertTrue(notes.startswith(original))
        self.assertEqual(notes.count(START), 1)
        self.assertIn('## 云服务器', notes)
        self.assertIn('## Unraid', notes)
        self.assertIn('/releases/download/v0.8.1/mfconnect.plg', notes)
        self.assertEqual(merge_notes(notes, self.version), notes)
        with self.assertRaises(ValueError):
            merge_notes(notes.replace(END, ''), self.version)


if __name__ == '__main__':
    unittest.main()
