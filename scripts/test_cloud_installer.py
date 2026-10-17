#!/usr/bin/env python3
"""Exercise installer boundaries without touching DNS, Docker or host services."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'deploy/install'))
import cloud


def inputs(host='connect.example.com'):
    return dict(entry_origin=host, zone='example.com', ipv4='8.8.8.8',
                secret_id='test-only-credential-id', secret_key='test-only-credential-key')


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='mfconnect-installer-test-')
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def test_apex_subdomain_and_invalid_inputs(self):
        for host, records in [('example.com', ['@', '*']),
                              ('connect.example.com', ['connect', '*.connect']),
                              ('a.b.example.com', ['a.b', '*.a.b'])]:
            c = cloud.settings(inputs(host))
            self.assertEqual(c['entry_origin'], 'https://' + host)
            self.assertEqual(cloud.manage.dns_records(c), records)
            self.assertEqual(c['sans'], [host, '*.' + host])
        for change in ({'entry_origin': 'example.com/api'}, {'entry_origin': 'other.test'},
                       {'entry_origin': 'https://name@example.com'}, {'ipv4': '192.168.1.2'},
                       {'entry_origin': 'example.com:443'}, {'ipv4': '::1'}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                cloud.settings({**inputs(), **change})
        with self.assertRaises(ValueError):
            cloud.secret_environment({**inputs(), 'secret_key': 'unsafe\nvalue'})

    def test_plan_never_prints_credentials_or_changes_host(self):
        config = self.directory / 'config.json'
        config.write_text(json.dumps(inputs()))
        config.chmod(0o600)
        result = subprocess.run([sys.executable, ROOT / 'deploy/install/cloud.py', '--package',
                                 self.directory, '--config', config, '--plan'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(json.loads(result.stdout)['applied'])
        self.assertNotIn('test-only-credential', result.stdout + result.stderr)
        self.assertEqual(list(self.directory.iterdir()), [config])
        config.chmod(0o644)
        result = subprocess.run([sys.executable, ROOT / 'deploy/install/cloud.py', '--package',
                                 self.directory, '--config', config, '--plan'], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('test-only-credential', result.stdout + result.stderr)

    def test_supported_systems(self):
        for distro, version in [('ubuntu', '24.04'), ('debian', '12'), ('opencloudos', '9.4')]:
            cloud.supported_system({'ID': distro, 'VERSION_ID': version})
        for distro, version in [('ubuntu', '20.04'), ('debian', '10'), ('opencloudos', '8.8'), ('arch', '')]:
            with self.assertRaises(RuntimeError):
                cloud.supported_system({'ID': distro, 'VERSION_ID': version})

    def test_nginx_conflicts_and_unrelated_site(self):
        include = self.directory / 'sites'; include.mkdir()
        def check(names, owned=None):
            text = f'http {{ include {include}/*.conf; }}\nserver_name {names};\n'
            with patch.object(cloud, 'run', return_value=subprocess.CompletedProcess([], 0, text, '')):
                return cloud.nginx_directory('/usr/sbin/nginx', 'connect.example.com', owned)
        self.assertEqual(check('www.example.com'), include / 'mfconnect.conf')
        for name in ('connect.example.com', '*.connect.example.com', 'nas.connect.example.com', '*.example.com', '~^.+example'):
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                check(name)
        (include / 'mfconnect.conf').write_text('user-owned')
        with self.assertRaises(RuntimeError): check('unrelated.test')
        self.assertEqual(check('unrelated.test', include / 'mfconnect.conf'), include / 'mfconnect.conf')

    def test_nginx_failed_validation_and_start_restore_previous_file(self):
        site = self.directory / 'site.conf'
        for previous in (None, 'original'):
            for failure in ('test', 'start'):
                if previous is not None: site.write_text(previous)
                def command(args, **kwargs):
                    if (failure == 'test' and '-t' in args) or (failure == 'start' and len(args) == 1):
                        raise RuntimeError('injected nginx failure')
                    return subprocess.CompletedProcess(args, 1 if 'reload' in args else 0, '', '')
                with patch.object(cloud, 'run', side_effect=command), self.assertRaises(RuntimeError):
                    cloud.install_nginx_site('/usr/sbin/nginx', site, 'candidate')
                self.assertEqual(site.read_text() if site.exists() else None, previous)

    def package(self):
        package = self.directory / 'package'
        runtime = package / 'runtime'; runtime.mkdir(parents=True)
        (runtime / '.env.example').write_bytes((ROOT / '.env.example').read_bytes())
        (runtime / 'compose.yaml').write_bytes((ROOT / 'compose.yaml').read_bytes())
        metadata = dict(image_id='sha256:' + '1' * 64, image='mfconnect-server:0.7.0-docker.1',
                        server_version='0.7.0', release_version='0.7.1', plugin_version='0.7.1',
                        plugin_payload='mfconnect-0.7.1-linux-amd64.tar.gz')
        (package / 'bundle.json').write_text(json.dumps(metadata))
        (package / 'plugin').mkdir()
        (package / 'plugin' / metadata['plugin_payload']).write_bytes(b'payload')
        (package / 'plugin/mfconnect.plg').write_text(cloud.PLACEHOLDER + '/payload')
        return package, metadata

    def host(self, root, metadata, *, fail_dns=False, occupied=False, volume=False):
        calls = []
        def command(args, **kwargs):
            args = [str(a) for a in args]; calls.append((args, kwargs))
            code, out = 0, ''
            if args[:3] == ['docker', 'image', 'inspect']: out = metadata['image_id']
            if args[:3] == ['docker', 'volume', 'inspect']: code = 0 if volume else 1
            if args[:3] == ['docker', 'network', 'inspect']:
                out = json.dumps([{'IPAM': {'Config': [{'Gateway': '172.22.0.1'}]}}])
            if '-c' in args and args[-1].startswith('test -s'): code = 1
            if 'dns' in args and fail_dns: raise RuntimeError('injected DNS error')
            if 'activate' in args:
                cert = root / 'current/certs'; cert.mkdir(exist_ok=True)
                (cert / 'fullchain.pem').write_text('test certificate')
            return subprocess.CompletedProcess(args, code, out, '')
        stack = contextlib.ExitStack()
        real_isdir = Path.is_dir
        stack.enter_context(patch.object(Path, 'is_dir', lambda p: True if str(p) == '/run/systemd/system' else real_isdir(p)))
        for name, value in [('operating_system', {'ID': 'debian', 'VERSION_ID': '12'}),
                            ('find_nginx', '/usr/sbin/nginx'), ('free_port', not occupied),
                            ('nginx_directory', self.directory / 'nginx.conf')]:
            stack.enter_context(patch.object(cloud, name, return_value=value))
        for name in ('install_dependencies', 'install_timer', 'configure_selinux', 'install_nginx_site'):
            stack.enter_context(patch.object(cloud, name))
        stack.enter_context(patch.object(cloud.os, 'geteuid', return_value=0))
        stack.enter_context(patch.object(cloud.os, 'chown'))
        stack.enter_context(patch.object(cloud.platform, 'machine', return_value='x86_64'))
        stack.enter_context(patch.object(cloud.shutil, 'which', side_effect=lambda name: '/usr/bin/docker' if name == 'docker' else None))
        stack.enter_context(patch.object(cloud.manage, 'verify_certificate'))
        stack.enter_context(patch.object(cloud, 'run', side_effect=command))
        return stack, calls

    def test_completed_install_layout_permissions_and_resume(self):
        package, metadata = self.package(); root = self.directory / 'installed'
        stack, calls = self.host(root, metadata)
        old_umask = os.umask(0o077)
        try:
            with stack, contextlib.redirect_stdout(io.StringIO()) as output:
                cloud.install(package, inputs(), root=root)
                count = len(calls)
                cloud.install(package, inputs(), root=root, resume=True)
                self.assertEqual(len(calls), count, 'completed installation must not be reinitialized')
        finally:
            os.umask(old_umask)
        self.assertNotIn('test-only-credential', output.getvalue())
        for name in ('public', 'public/downloads', 'public/downloads/mfconnect'):
            self.assertEqual((root / name).stat().st_mode & 0o777, 0o755)
        for name in ('secrets/dns.env', 'secrets/setup-input.json', 'current/.env'):
            self.assertEqual((root / name).stat().st_mode & 0o777, 0o600)
        self.assertEqual((root / 'updates').stat().st_mode & 0o777, 0o700)
        manifest = (root / 'public/downloads/mfconnect/mfconnect.plg').read_text()
        self.assertIn('https://connect.example.com/downloads/mfconnect/', manifest)
        self.assertNotIn(cloud.PLACEHOLDER, manifest)
        self.assertIn('MF_TRUSTED_PROXY_IPS=["172.22.0.1"]', (root / 'current/.env').read_text())
        commands = [args for args, _ in calls]
        self.assertTrue(any(args[-2:] == ['mfconnect', 'init'] for args in commands))
        self.assertTrue(any('--resolve' in args for args in commands))
        self.assertFalse(any('build' in args or 'down' in args for args in commands))

    def test_partial_install_resumes_and_mismatched_domain_is_rejected(self):
        package, metadata = self.package(); root = self.directory / 'installed'
        stack, _ = self.host(root, metadata, fail_dns=True)
        with stack, contextlib.redirect_stdout(io.StringIO()), self.assertRaises(RuntimeError):
            cloud.install(package, inputs(), root=root)
        self.assertTrue((root / 'installer-state.json').is_file())
        stack, _ = self.host(root, metadata)
        with stack, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(RuntimeError): cloud.install(package, inputs('other.example.com'), root=root, resume=True)
            cloud.install(package, inputs(), root=root, resume=True)
        self.assertTrue((root / 'installation.json').is_file())

    def test_existing_data_volume_and_ports_prevent_mutation(self):
        package, metadata = self.package()
        for scenario in ('directory', 'volume', 'port'):
            root = self.directory / scenario
            if scenario == 'directory':
                root.mkdir(); (root / 'keep').write_text('user data')
            stack, calls = self.host(root, metadata, volume=scenario == 'volume', occupied=scenario == 'port')
            with stack, self.assertRaises(RuntimeError): cloud.install(package, inputs(), root=root)
            self.assertFalse((root / 'installer-state.json').exists())
            self.assertFalse(any('load' in args for args, _ in calls))
        self.assertEqual((self.directory / 'directory/keep').read_text(), 'user data')

    def test_acme_issue_already_valid_still_installs_and_renew_skips(self):
        c = cloud.settings(inputs(), self.directory)
        with patch.object(cloud.manage.subprocess, 'run', return_value=subprocess.CompletedProcess([], 2)):
            with patch.object(cloud.manage, 'run') as install_cert:
                self.assertTrue(cloud.manage.certificate(c))
                self.assertIn('--install-cert', install_cert.call_args.args[0])
                install_cert.reset_mock()
                self.assertFalse(cloud.manage.certificate(c, renew=True))
                install_cert.assert_not_called()
        c['nginx_binary'] = '/www/server/nginx/sbin/nginx'
        with patch.object(cloud.manage, 'run') as command, patch.object(cloud.manage.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)):
            cloud.manage.reload_services(c)
        self.assertEqual(command.call_args_list[0].args[0], [c['nginx_binary'], '-t'])
        self.assertEqual(command.call_args_list[-1].args[0], [c['nginx_binary'], '-s', 'reload'])

    def test_bootstrap_checksum_and_archive_traversal_protection(self):
        source = (ROOT / 'install.sh').read_text().split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
        for scenario in ('valid', 'checksum', 'traversal', 'symlink'):
            folder = self.directory / scenario; folder.mkdir()
            archive = folder / 'bundle.tar.gz'
            with tarfile.open(archive, 'w:gz') as output:
                member = tarfile.TarInfo('../outside' if scenario == 'traversal' else 'deploy/file')
                if scenario == 'symlink': member.type, member.linkname = tarfile.SYMTYPE, '/etc'
                else: member.size = 4
                output.addfile(member, io.BytesIO(b'data'))
            digest = '0' * 64 if scenario == 'checksum' else hashlib.sha256(archive.read_bytes()).hexdigest()
            (folder / 'SHA256SUMS').write_text(digest + '  bundle.tar.gz\n')
            result = subprocess.run([sys.executable, '-', str(folder), archive.name], input=source, capture_output=True, text=True)
            self.assertEqual(result.returncode == 0, scenario == 'valid', result.stderr)
            self.assertFalse((folder / 'outside').exists())

    def test_release_bundle_uses_allowlist_and_retains_dependency_source(self):
        spec = importlib.util.spec_from_file_location('package_release', ROOT / 'scripts/package_release.py')
        builder = importlib.util.module_from_spec(spec); spec.loader.exec_module(builder)
        fixture = self.directory / 'source'; fixture.mkdir()
        for name in (*builder.RUNTIME_FILES, 'deploy/install/cloud.py', 'README.md', 'docs/deployment.md'):
            target = fixture / name; target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((ROOT / name).read_bytes() if (ROOT / name).is_file() else b'fixture')
        binary = fixture / 'dist/mfconnect-server'
        binary.write_text('#!/bin/sh\necho 0.7.0\n'); binary.chmod(0o755)
        marker = b'PRIVATE_INSTALLER_FIXTURE_STAYS_LOCAL'
        (fixture / '.env').write_bytes(marker)
        (fixture / 'deploy/unified/private.json').write_bytes(marker)
        (fixture / 'dist/private-notes.md').write_bytes(marker)
        plugin = fixture / 'plugin'; plugin.mkdir()
        payload = plugin / 'mfconnect-0.7.1-linux-amd64.tar.gz'; payload.write_bytes(b'plugin fixture')
        (plugin / 'mfconnect.plg').write_text(cloud.PLACEHOLDER + '/' + payload.name)
        (plugin / 'release.json').write_text(json.dumps(dict(version='0.7.1', payload=payload.name,
            release_base=cloud.PLACEHOLDER, sha256=hashlib.sha256(payload.read_bytes()).hexdigest())))
        image = fixture / 'image.tar.gz'; image.write_bytes(b'image fixture')
        acme = fixture / 'acme.tar.gz'
        with tarfile.open(acme, 'w:gz') as archive:
            for name in ('acme.sh', 'LICENSE.md', 'dnsapi/dns_tencent.sh'):
                item = tarfile.TarInfo('acme.sh-3.1.2/' + name); item.size = 7
                archive.addfile(item, io.BytesIO(b'fixture'))
        output = fixture / 'output'
        argv = ['package_release', '--version', '0.7.1', '--image', str(image), '--image-id', 'sha256:' + '1' * 64,
                '--plugin-dir', str(plugin), '--acme-archive', str(acme), '--output', str(output)]
        with patch.object(builder, 'ROOT', fixture), patch.object(builder, 'ACME_SHA256', hashlib.sha256(acme.read_bytes()).hexdigest()), patch.object(sys, 'argv', argv):
            with contextlib.redirect_stdout(io.StringIO()): builder.main()
        with tarfile.open(output / 'mfconnect-0.7.1-linux-amd64-deploy.tar.gz') as archive:
            self.assertIn('third_party/acme.sh/LICENSE.md', archive.getnames())
            self.assertIn('third_party/acme.sh/dnsapi/dns_tencent.sh', archive.getnames())
            for member in archive:
                self.assertTrue(member.isfile())
                self.assertNotIn(marker, archive.extractfile(member).read(), member.name)


if __name__ == '__main__':
    unittest.main()
