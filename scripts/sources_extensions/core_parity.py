#!/usr/bin/env python3
"""Exact-path/hash ledger for approved core deviations, not a blanket fork allowlist."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from sources_baseline import load_baseline
UPSTREAM = load_baseline()['commit']

def git(*args):
    return subprocess.check_output(['git', '-C', str(ROOT), *args]).decode().splitlines()

def check(root=ROOT):
    manifest = json.loads((root / 'docs/sources/CORE-PATCHES.json').read_text())
    assert manifest['upstream'] == UPSTREAM
    changed = set(git('diff', '--name-only', UPSTREAM, '--', 'backend', 'frontend', 'Dockerfile'))
    changed.update(git('ls-files', '--others', '--exclude-standard', '--', 'backend', 'frontend', 'Dockerfile'))
    expected = {item['path']: item for item in manifest['files']}
    assert changed == set(expected), f'unreviewed/missing core paths: {sorted(changed ^ set(expected))}'
    for path, record in expected.items():
        assert hashlib.sha256((root / path).read_bytes()).hexdigest() == record['sha256'], f'core content changed after review: {path}'
    print(f'CORE_PARITY_PASS: {len(expected)} approved core/build paths; monitor timing, entry policy, local appearance, performance display, verified native payments and media engine boundary; other core files equal pinned upstream')

if __name__ == '__main__':
    check()
