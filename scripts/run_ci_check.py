#!/usr/bin/env python3
"""Surface controlled fixture failures as CI annotations, without private hosts."""
import os
import subprocess
import sys

result = subprocess.run(sys.argv[1:], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
print(result.stdout, end='', flush=True)
if result.returncode:
    excerpt = result.stdout[-10000:]
    summary = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary:
        with open(summary, 'a') as output: output.write('Controlled test output:\n\n```text\n' + excerpt + '\n```\n')
    print('::error title=Controlled test failed::' + excerpt.replace('%','%25').replace('\r','%0D').replace('\n','%0A'))
raise SystemExit(result.returncode)
