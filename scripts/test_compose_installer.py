#!/usr/bin/env python3
"""Portable deployment boundaries; no cloud account or host service changes."""
import contextlib
import hashlib
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
sys.path.insert(0, str(ROOT / 'deploy/gateway'))
import gateway
import package_gateway


class ComposeInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='mfconnect-compose-unit-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def configuration(self, host='example.test', **extra):
        return gateway.settings({'MF_ENTRY_ORIGIN': 'https://' + host, **extra}, self.root / 'state')

    def test_default_requires_only_entry_and_existing_certificate(self):
        for host in ('example.test', 'connect.example.test', 'a.b.example.test'):
            c = self.configuration(host)
            self.assertEqual(c['tls_mode'], 'provided')
            self.assertEqual(c['sans'], [host, '*.' + host])
            self.assertEqual(gateway.credentials(c, self.root / 'nonexistent'), {})
            rendered = gateway.nginx_config(c)
            self.assertIn('http {\naccess_log off;', rendered)
            self.assertIn('resolver 127.0.0.11', rendered)
            self.assertIn('proxy_ssl_name $host;', rendered)
            self.assertIn('proxy_pass https://$mf_upstream;', rendered)
        for host in ('example.test/api', 'example.test:443', 'user@example.test'):
            with self.assertRaises(ValueError): self.configuration(host)

    def test_optional_providers_are_explicit_and_isolated(self):
        for provider, hook in [('cloudflare','dns_cf'), ('alidns','dns_ali'), ('dnspod','dns_tencent')]:
            c = self.configuration(MF_TLS_MODE='acme', MF_DNS_PROVIDER=provider)
            self.assertEqual(c['acme_dns'], hook)
            required = gateway.PROVIDERS[provider][1]
            secret = self.root / 'dns.env'
            secret.write_text(''.join(name + '=test-only-credential\n' for name in required)); secret.chmod(0o600)
            values = gateway.credentials(c, secret)
            with gateway.dns_environment(values):
                for key in required: self.assertEqual(os.environ[key], 'test-only-credential')
            for key in required: self.assertNotIn(key, os.environ)
            with patch.object(gateway.manage.subprocess, 'run', return_value=subprocess.CompletedProcess([],0)) as command, patch.object(gateway.manage, 'run'):
                gateway.manage.certificate(c)
            self.assertIn(hook, command.call_args.args[0])
        with self.assertRaises(ValueError): self.configuration(MF_TLS_MODE='acme')
        for text in ('PATH=/malicious\n', 'CF_Token=test\nCF_Token=second\n', 'CF_Token=$(example)\n'):
            secret.write_text(text)
            with self.assertRaises(ValueError): gateway.credentials(self.configuration(MF_TLS_MODE='acme',MF_DNS_PROVIDER='cloudflare'),secret)
        secret.write_text('CF_Token=test\n'); secret.chmod(0o644)
        with self.assertRaises(ValueError): gateway.credentials(self.configuration(MF_TLS_MODE='acme',MF_DNS_PROVIDER='cloudflare'),secret)

    def plugin(self):
        assets = self.root / 'assets'; assets.mkdir()
        payload = 'mfconnect-0.8.1-linux-amd64.tar.gz'
        (assets / payload).write_bytes(b'payload')
        (assets / 'release.json').write_text(json.dumps(dict(payload=payload,version='0.8.1',
            sha256=hashlib.sha256(b'payload').hexdigest(), release_base=gateway.PLACEHOLDER)))
        (assets / 'mfconnect.plg').write_text(gateway.PLACEHOLDER + '/' + payload)
        return assets

    def test_plugin_downloads_are_readable_but_private_state_is_not_published(self):
        c = self.configuration(); assets = self.plugin(); updates = self.root / 'updates'
        previous = os.umask(0o077)
        try:
            with patch.object(gateway.os, 'chown'): gateway.publish(c, assets, updates)
        finally: os.umask(previous)
        for name in ('state', 'state/public', 'state/public/downloads', 'state/public/downloads/mfconnect'):
            self.assertEqual((self.root / name).stat().st_mode & 0o777, 0o755)
        self.assertEqual(updates.stat().st_mode & 0o777, 0o700)
        self.assertIn('https://example.test/downloads/mfconnect', (updates / 'plugin-install.plg').read_text())
        (assets / 'mfconnect-0.8.1-linux-amd64.tar.gz').write_bytes(b'corrupt')
        with patch.object(gateway.os, 'chown'), self.assertRaises(ValueError): gateway.publish(c, assets, updates)

    def test_certificate_replacement_is_verified_before_activation(self):
        c = self.configuration(); provided = self.root / 'config'; provided.mkdir()
        for name in ('fullchain.pem','privkey.pem'): (provided / name).write_text('initial-' + name)
        with patch.object(gateway.manage,'verify_certificate'), patch.object(gateway.os,'chown'):
            self.assertTrue(gateway.prepare_tls(c, provided))
            self.assertFalse(gateway.prepare_tls(c, provided))
            previous = os.readlink(self.root / 'state/certs/live')
            (provided / 'privkey.pem').write_text('invalid-key')
            with patch.object(gateway.manage,'verify_certificate',side_effect=ValueError('key mismatch')), self.assertRaises(ValueError):
                gateway.prepare_tls(c, provided)
        self.assertEqual(os.readlink(self.root / 'state/certs/live'), previous)

    def test_wizard_does_not_require_a_provider_or_overwrite_existing_configuration(self):
        with patch('builtins.input',side_effect=['connect.example.com','']), contextlib.redirect_stdout(io.StringIO()) as output:
            gateway.configure(self.root)
        self.assertIn('MF_TLS_MODE=provided', (self.root / '.env').read_text())
        self.assertNotIn('Tencent', (self.root / '.env').read_text())
        self.assertFalse((self.root / 'config/dns.env').exists())
        self.assertEqual((self.root / '.env').stat().st_mode & 0o777, 0o600)
        with self.assertRaises(ValueError): gateway.configure(self.root)

    def test_gateway_build_context_allowlist_and_dependency_license(self):
        assets = self.plugin(); (assets / 'private.env').write_text('PRIVATE_FIXTURE')
        blob = io.BytesIO()
        with tarfile.open(fileobj=blob,mode='w:gz') as archive:
            for name in ('acme.sh','LICENSE.md','dnsapi/dns_cf.sh'):
                item = tarfile.TarInfo('acme.sh-3.1.2/' + name); item.size = 7
                archive.addfile(item,io.BytesIO(b'fixture'))
        data = blob.getvalue(); out = self.root / 'context'
        with patch.object(package_gateway,'ACME_SHA256',hashlib.sha256(data).hexdigest()), contextlib.redirect_stdout(io.StringIO()):
            package_gateway.stage(out, assets, data)
            with self.assertRaises(ValueError): package_gateway.stage(out, assets, data)
        self.assertTrue((out / 'acme/LICENSE.md').is_file())
        for path in out.rglob('*'):
            if path.is_file(): self.assertNotIn(b'PRIVATE_FIXTURE',path.read_bytes())

    def test_bootstrap_is_optional_and_installs_no_host_services(self):
        result = subprocess.run(['sh', ROOT / 'install.sh', '--help'],capture_output=True,text=True,check=True)
        self.assertIn('Installs no host services',result.stdout)
        source = (ROOT / 'install.sh').read_text()
        for forbidden in ('apt-get','dnf ', 'systemctl','/etc/nginx','sudo ', 'python3 '):
            self.assertNotIn(forbidden,source)
        subprocess.run(['sh','-n',ROOT / 'install.sh'],check=True)
        subprocess.run(['sh','-n',ROOT / 'deploy/docker/run.sh'],check=True)
        subprocess.run(['sh','-n',ROOT / 'deploy/docker/watch-tls.sh'],check=True)

    def test_plain_docker_does_not_execute_env_values_or_touch_unowned_resources(self):
        launcher = self.root / 'docker-run.sh'
        launcher.write_bytes((ROOT / 'deploy/docker/run.sh').read_bytes())
        marker = self.root / 'executed'
        (self.root / '.env').write_text('MF_ENTRY_ORIGIN=https://example.test\nMF_SERVER_IMAGE=$(touch ' + str(marker) + ')\n')
        tools = self.root / 'bin'; tools.mkdir()
        docker = tools / 'docker'
        docker.write_text('#!/bin/sh\ncase "$1" in info) exit 0;; container) echo unrelated-owner; exit 0;; *) exit 99;; esac\n')
        docker.chmod(0o755)
        result = subprocess.run(['sh',launcher,'down'],env={**os.environ,'PATH':str(tools) + ':' + os.environ['PATH']},capture_output=True,text=True)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('Refusing to modify an unrelated container',result.stderr)
        self.assertFalse(marker.exists())

    def test_deployment_archive_excludes_images_and_offline_bundle_contains_them(self):
        images = self.root / 'images.tar.gz'; images.write_bytes(b'test-image-archive')
        output = self.root / 'release'
        subprocess.run([sys.executable,ROOT / 'scripts/package_release.py','--version','0.8.1',
                        '--images',images,'--server-image-id','sha256:' + '1'*64,
                        '--gateway-image-id','sha256:' + '2'*64,'--output',output],check=True,stdout=subprocess.PIPE)
        for suffix in ('deploy','docker-amd64'):
            with tarfile.open(output / ('mfconnect-0.8.1-' + suffix + '.tar.gz')) as archive:
                names = archive.getnames()
                self.assertIn('mfconnect/docker-run.sh',names)
                self.assertIn('mfconnect/compose.yaml',names)
                self.assertEqual('mfconnect/images.tar.gz' in names,suffix == 'docker-amd64')
                manifest = archive.extractfile('mfconnect/CONTENTS.sha256').read().decode()
                for line in manifest.splitlines():
                    digest, name = line.split('  ',1)
                    self.assertEqual(hashlib.sha256(archive.extractfile('mfconnect/' + name).read()).hexdigest(),digest)


if __name__ == '__main__': unittest.main()
