#!/usr/bin/env python3
"""Check a staged tree, exported directory, or reachable history before publishing.

The optional denylist is a private local file, one literal per line. Findings
show only the file/commit and rule, never the matched credential or address.
"""
import argparse
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

TOP_LEVEL = {'.dockerignore', '.env.example', '.gitignore', 'README.md',
             'Makefile', 'Dockerfile', 'compose.yaml', 'go.mod', 'go.sum',
             'LICENSE', 'NOTICE', 'install.sh'}
DOCUMENTS = {'docs/api.md', 'docs/deployment.md', 'docs/development.md'}
SOURCE_SUFFIXES = {
    'cmd': {'.go'}, 'internal': {'.go'},
    'plugin': {'.php', '.page', '.css', '.js', '.sh'},
    'deploy': {'.sh', '.py', '.json', '.service', '.timer'},
    'scripts': {'.py', '.sh', '.php'},
}
SECRETS = (
    ('private key', re.compile(rb'-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----')),
    ('GitHub credential', re.compile(rb'(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})')),
    ('cloud credential', re.compile(rb'(?:AKID[A-Za-z0-9]{20,}|AKIA[A-Z0-9]{16})')),
)


def allowed_path(name):
    if name in TOP_LEVEL or name in DOCUMENTS or name in ('plugin/rc.mfconnect', '.github/workflows/release.yml'):
        return True
    path = PurePosixPath(name)
    return (len(path.parts) > 1
            and not any(part.startswith('.') or part in ('secrets', 'backups', '__pycache__')
                        for part in path.parts)
            and path.suffix in SOURCE_SUFFIXES.get(path.parts[0], set()))


def check_data(label, data, denylist):
    issues = [f'{label}: {rule}' for rule, pattern in SECRETS if pattern.search(data)]
    if any(value in data for value in denylist):
        issues.append(f'{label}: private denylist match')
    return issues


def git(repo, *args):
    return subprocess.check_output(['git', '-C', str(repo), *args], stderr=subprocess.PIPE)


def audit(repo, denylist, directory=None, ref=None):
    issues, count, seen = [], 0, set()

    def inspect(name, data, label=None):
        nonlocal count
        count += 1
        if not allowed_path(name):
            issues.append(f'{label or name}: file outside public source scope')
        issues.extend(check_data(label or name, data, denylist))

    if directory:
        for path in sorted(directory.rglob('*')):
            name = path.relative_to(directory).as_posix()
            if '.git' in PurePosixPath(name).parts:
                continue
            if path.is_symlink():
                issues.append(f'{name}: symlink in publication tree')
            elif path.is_file():
                inspect(name, path.read_bytes())
    elif ref:
        for commit in git(repo, 'rev-list', ref).decode().splitlines():
            metadata = git(repo, 'show', '-s', '--format=%an <%ae>%n%cn <%ce>%n%B', commit)
            issues.extend(check_data(commit[:12] + ' metadata', metadata, denylist))
            for record in git(repo, 'ls-tree', '-rz', commit).split(b'\0'):
                if not record:
                    continue
                entry, raw_name = record.split(b'\t', 1)
                mode, kind, object_id = entry.decode().split()
                name = raw_name.decode()
                if (name, object_id) in seen:
                    continue
                seen.add((name, object_id))
                if kind != 'blob' or mode not in ('100644', '100755'):
                    issues.append(f'{name}: non-regular entry in history')
                    continue
                inspect(name, git(repo, 'cat-file', 'blob', object_id), commit[:12] + ':' + name)
    else:
        # Read the staged content, not a potentially different working copy.
        for record in git(repo, 'ls-files', '--stage', '-z').split(b'\0'):
            if not record:
                continue
            entry, raw_name = record.split(b'\t', 1)
            mode, object_id, stage = entry.decode().split()
            name = raw_name.decode()
            if stage != '0' or mode not in ('100644', '100755'):
                issues.append(f'{name}: unresolved or non-regular index entry')
                continue
            inspect(name, git(repo, 'cat-file', 'blob', object_id))
    if not count:
        issues.append('publication tree is empty')
    return count, issues


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, default=Path.cwd())
    parser.add_argument('--denylist', type=Path)
    source = parser.add_mutually_exclusive_group()
    source.add_argument('--directory', type=Path)
    source.add_argument('--ref', help='Check every commit reachable from this ref')
    args = parser.parse_args()
    try:
        denylist = [] if args.denylist is None else [line.strip() for line in args.denylist.read_bytes().splitlines()
                                                    if line.strip() and not line.startswith(b'#')]
        count, issues = audit(args.repo, denylist, args.directory, args.ref)
    except (OSError, subprocess.CalledProcessError) as error:
        print(f'Publication check could not complete ({type(error).__name__}).', file=sys.stderr)
        return 2
    for issue in issues:
        print(issue, file=sys.stderr)
    print(f'Checked {count} file versions; {len(issues)} findings.')
    return 1 if issues else 0


if __name__ == '__main__':
    raise SystemExit(main())
