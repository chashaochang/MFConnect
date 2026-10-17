#!/usr/bin/env python3
"""Sign an immutable release entry; never copy the signing key into artifacts."""
import argparse, base64, hashlib, json, time
from pathlib import Path
from urllib.parse import urlsplit
from cryptography.hazmat.primitives import serialization

p = argparse.ArgumentParser()
p.add_argument('--component', choices=['plugin', 'server'], required=True)
p.add_argument('--version', required=True)
p.add_argument('--artifact', type=Path)
p.add_argument('--sha256')
p.add_argument('--size',type=int)
p.add_argument('--url', required=True)
p.add_argument('--image-id', default='')
p.add_argument('--notes', required=True)
p.add_argument('--key', type=Path, default=Path('dist/signing/release-ed25519.pem'))
p.add_argument('--output', type=Path, required=True)
p.add_argument('--catalog', type=Path)
a = p.parse_args()
if a.artifact:
    digest=hashlib.file_digest(a.artifact.open('rb'),'sha256').hexdigest();size=a.artifact.stat().st_size
else:
    if not a.sha256 or a.size is None:p.error('provide --artifact or both --sha256 and --size from the trusted build host')
    digest,size=a.sha256,a.size
if a.component == 'plugin' and (not urlsplit(a.url).path.endswith('.plg') or size > 1 << 20):
    p.error('plugin updates must sign the versioned .plg manifest; its payload checksum is embedded in that manifest')
key = serialization.load_pem_private_key(a.key.read_bytes(), password=None)
now = int(time.time())
data = dict(id=f'{a.component}-{a.version}', component=a.component, version=a.version,
            architecture='linux/amd64', protocol=1, url=a.url, sha256=digest,
            size=size, notes=a.notes, issued_at=now, expires_at=now+180*86400)
if a.component == 'server': data.update(image=f'mfconnect-server:{a.version}-docker.1',image_id=a.image_id)
payload = json.dumps(data, ensure_ascii=False, separators=(',',':')).encode()
signed = dict(payload=base64.b64encode(payload).decode(),signature=base64.b64encode(key.sign(payload)).decode())
a.output.parent.mkdir(parents=True,exist_ok=True)
a.output.write_text(json.dumps(signed,indent=2)+'\n')
if a.catalog:
    catalog = json.loads(a.catalog.read_text()) if a.catalog.exists() else {'releases':[]}
    catalog['releases'] = [entry for entry in catalog['releases'] if json.loads(base64.b64decode(entry['payload']))['component'] != a.component]
    catalog['releases'].append(signed)
    a.catalog.write_text(json.dumps(catalog,indent=2)+'\n')
print(f'Signed {data["id"]}: {a.output}')
