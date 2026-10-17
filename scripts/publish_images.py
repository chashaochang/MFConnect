#!/usr/bin/env python3
"""Publish tested images without replacing tags; require anonymous release pulls."""
import base64
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request


def registry_token(package):
    query = urllib.parse.urlencode({'service': 'ghcr.io', 'scope': 'repository:' + package + ':pull,push'})
    credentials = (os.environ['GITHUB_ACTOR'] + ':' + os.environ['GH_TOKEN']).encode()
    request = urllib.request.Request('https://ghcr.io/token?' + query,
        headers={'Authorization': 'Basic ' + base64.b64encode(credentials).decode()})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)['token']


def existing_config(package, tag, token):
    request = urllib.request.Request('https://ghcr.io/v2/' + package + '/manifests/' + tag,
        headers={'Authorization': 'Bearer ' + token,
                 'Accept': 'application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json'})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)['config']['digest']
    except urllib.error.HTTPError as error:
        if error.code == 404: return None
        raise


def main():
    version = subprocess.check_output(['dist/mfconnect-server', 'version'], text=True).strip()
    if not re.fullmatch(r'\d+\.\d+\.\d+', version): raise ValueError('Invalid release version')
    stable = os.environ['GITHUB_REF'].startswith('refs/tags/')
    if stable and os.environ['GITHUB_REF_NAME'] != 'v' + version:
        raise ValueError('Release tag must match compiled version')
    # This publisher belongs to the public upstream. Forks must explicitly
    # configure their own namespace and deployment image references first.
    if os.environ['GITHUB_REPOSITORY'] != 'chashaochang/MFConnect':
        raise ValueError('Configure the registry namespace before publishing from a fork')
    with tempfile.TemporaryDirectory(prefix='mfconnect-registry-') as directory:
        private = ['docker', '--config', directory + '/authenticated']
        public = ['docker', '--config', directory + '/anonymous']
        subprocess.run(private + ['login', 'ghcr.io', '-u', os.environ['GITHUB_ACTOR'], '--password-stdin'],
                       input=os.environ['GH_TOKEN'], text=True, check=True, stdout=subprocess.DEVNULL)
        available = True
        for component, local_tag in [('server', version + '-docker.1'), ('gateway', version)]:
            package = 'chashaochang/mfconnect-' + component
            tag = local_tag if stable else 'candidate-' + os.environ['GITHUB_SHA']
            target = 'ghcr.io/' + package + ':' + tag
            source = 'mfconnect-' + component + ':' + local_tag
            identity = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', source], text=True).strip()
            previous = existing_config(package, tag, registry_token(package))
            if previous is not None and previous != identity:
                raise ValueError('Refusing to replace an existing image tag: ' + target)
            subprocess.run(['docker', 'tag', source, target], check=True)
            if previous is None: subprocess.run(private + ['push', target], check=True)
            result = subprocess.run(public + ['pull', target], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            if result.returncode:
                available = False
                print('::warning::Anonymous pull unavailable for ' + target + '; package owner must enable public visibility.', flush=True)
            else:
                print('Verified anonymous pull: ' + target, flush=True)
            if os.environ.get('GITHUB_STEP_SUMMARY'):
                with Path(os.environ['GITHUB_STEP_SUMMARY']).open('a') as output:
                    output.write('- `' + target + '`: published; anonymous pull ' + ('passed' if result.returncode == 0 else 'unavailable') + '\n')
        if stable and not available:
            raise RuntimeError('Release is blocked until both images can be pulled without credentials')


if __name__ == '__main__': main()
