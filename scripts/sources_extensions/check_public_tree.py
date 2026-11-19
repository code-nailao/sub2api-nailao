#!/usr/bin/env python3
"""Checks that hold for every change to this public tree.

Both checks cover the fork's own documentation and tooling. Upstream files are
published by upstream as they are, with their own link conventions and contact
addresses, and stay byte-identical here.

1. Relative Markdown links resolve to a tracked file or directory.
2. No detail of a real host: public IP addresses, personal home-directory
   paths, personal email addresses. Operations records belong in the private
   operations repository, not here.

The patterns are deliberately generic. A list of the specific names to keep out
would itself disclose them, so that list is not part of this repository.
"""
import ipaddress
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

FORK_OWNED = (
    'AGENTS.md', 'SOURCES.md', 'DEV_GUIDE.md', 'CHANGELOG.md', 'NOTICE',
    '.sources-identity.json', '.github/README.md', '.github/workflows/',
    '.github/release-tools/', 'docs/sources/', 'scripts/', 'deploy/sources/',
    'deploy/clean/', 'extensions/',
)
# Generated or third-party content inside the fork-owned areas.
SKIPPED = ('docs/sources/CORE-PATCHES.json', 'node_modules/', '/dist/', 'pnpm-lock.yaml', 'go.sum')
TEXT_SUFFIXES = {'.md', '.py', '.sh', '.yml', '.yaml', '.json', '.txt', '.service', '.conf', '.toml', ''}

LINK = re.compile(r'\[[^\]\n]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"\n]*")?\s*\)')
FENCE = re.compile(r'^\s*(```|~~~)')
SCHEME = re.compile(r'^[A-Za-z][A-Za-z0-9+.-]*:')
IPV4 = re.compile(r'(?<![\w.])(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(?![\w.]|/\d)')
HOME = re.compile(r'(/Users/|/home/)[A-Za-z0-9._-]+/')
EMAIL = re.compile(r'[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}')
EMAIL_OK = re.compile(r'(noreply|example\.(com|org|net)|@users\.noreply\.github\.com|^git@)', re.I)


def tracked():
    out = subprocess.check_output(['git', '-C', str(ROOT), 'ls-files'], text=True)
    return [line for line in out.splitlines() if line]


def broken_links(files):
    known = set(files)
    directories = {str(Path(f).parent) for f in files}
    for name in files:
        if not name.endswith('.md') or not name.startswith(FORK_OWNED):
            continue
        fenced = False
        for number, line in enumerate((ROOT / name).read_text(errors='replace').splitlines(), 1):
            if FENCE.match(line):
                fenced = not fenced
                continue
            if fenced:
                continue
            for target in LINK.findall(line):
                if SCHEME.match(target) or target.startswith('#'):
                    continue
                path = target.split('#', 1)[0].split('?', 1)[0]
                if not path:
                    continue
                base = ROOT if path.startswith('/') else (ROOT / name).parent
                resolved = (base / path.lstrip('/')).resolve()
                try:
                    relative = str(resolved.relative_to(ROOT))
                except ValueError:
                    yield f'{name}:{number}: link leaves the repository: {target}'
                    continue
                if relative not in known and relative not in directories and relative != '.':
                    yield f'{name}:{number}: broken link: {target}'


def public_address(text):
    try:
        address = ipaddress.ip_address(text)
    except ValueError:
        return False
    return address.is_global


def host_details(files):
    for name in files:
        if not name.startswith(FORK_OWNED) or any(part in name for part in SKIPPED):
            continue
        if Path(name).suffix not in TEXT_SUFFIXES:
            continue
        path = ROOT / name
        if path.is_symlink() or path.stat().st_size > 1_000_000:
            continue
        if path.resolve() == Path(__file__).resolve():
            continue
        for number, line in enumerate(path.read_text(errors='replace').splitlines(), 1):
            for match in IPV4.findall(line):
                if public_address(match):
                    yield f'{name}:{number}: public IP address'
            if HOME.search(line):
                yield f'{name}:{number}: personal home-directory path'
            for match in EMAIL.findall(line):
                if not EMAIL_OK.search(match):
                    yield f'{name}:{number}: email address'


def main():
    files = tracked()
    problems = list(broken_links(files)) + list(host_details(files))
    for problem in problems:
        print(problem)
    if problems:
        print(f'PUBLIC_TREE_FAIL: {len(problems)} problem(s)')
        return 1
    print(f'PUBLIC_TREE_PASS: {len(files)} tracked files; fork-owned Markdown links resolve and carry no host details')
    return 0


if __name__ == '__main__':
    sys.exit(main())
