#!/usr/bin/env python3
"""Publication checks must cover staged bytes, old history and author metadata."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from check_publication import audit


class PublicationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='mfconnect-publication-')
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.env = {**os.environ, 'GIT_AUTHOR_NAME': 'Test', 'GIT_COMMITTER_NAME': 'Test',
                    'GIT_AUTHOR_EMAIL': 'test@example.invalid', 'GIT_COMMITTER_EMAIL': 'test@example.invalid'}
        self.git('init', '-q')

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.repo), *args], env=self.env, stderr=subprocess.PIPE)

    def write(self, name, value):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value)

    def test_staged_secret_is_checked_even_if_working_file_is_clean(self):
        self.write('README.md', 'private-deployment-marker')
        self.git('add', 'README.md')
        self.write('README.md', '# Safe working copy')
        count, issues = audit(self.repo, [b'private-deployment-marker'])
        self.assertEqual(count, 1)
        self.assertTrue(issues)
        self.assertNotIn('private-deployment-marker', '\n'.join(issues))

    def test_history_includes_deleted_file_and_author(self):
        self.write('README.md', '# Project')
        self.write('DEPLOYED.md', 'private-deployment-marker')
        self.git('add', '.')
        self.git('commit', '-qm', 'initial')
        self.git('rm', '-q', 'DEPLOYED.md')
        self.git('commit', '-qm', 'remove notes')
        _, issues = audit(self.repo, [b'private-deployment-marker', b'test@example.invalid'], ref='HEAD')
        self.assertTrue(any('DEPLOYED.md' in issue for issue in issues))
        self.assertTrue(any('metadata' in issue for issue in issues))

    def test_tracked_ignored_private_file_is_rejected(self):
        self.write('.gitignore', '.env\n')
        self.write('.env', 'VALUE=private')
        self.git('add', '-f', '.env', '.gitignore')
        _, issues = audit(self.repo, [])
        self.assertTrue(any('.env:' in issue for issue in issues))

    def test_private_key_in_allowed_source_file_is_rejected(self):
        self.write('README.md', '-----BEGIN ' + 'PRIVATE KEY-----\nfixture\n')
        _, issues = audit(self.repo, [], directory=self.repo)
        self.assertTrue(any('private key' in issue for issue in issues))

    def test_reviewed_source_tree_is_accepted(self):
        self.write('README.md', '# Project\nUse https://example.com.\n')
        self.write('docs/api.md', '# API')
        self.write('cmd/example/main.go', 'package main\n')
        self.git('add', '.')
        self.assertEqual(audit(self.repo, [b'private-deployment-marker'])[1], [])


if __name__ == '__main__':
    unittest.main()
