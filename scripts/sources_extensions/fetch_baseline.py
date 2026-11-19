#!/usr/bin/env python3
"""Fetch only the reviewed stable tag into a private ref and verify its commit.

Patch-based upstream syncs do not make the upstream commit an ancestor, so a
fresh CI checkout needs this object before exact core parity can be checked.
"""
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from sources_baseline import load_baseline


def main():
    baseline = load_baseline()
    ref = 'refs/sources-upstream/' + baseline['version']
    subprocess.run(['git', '-C', str(ROOT), 'fetch', '--no-tags', baseline['repository'] + '.git',
                    'refs/tags/' + baseline['version'] + ':' + ref], check=True)
    actual = subprocess.check_output(['git', '-C', str(ROOT), 'rev-parse', ref + '^{commit}'], text=True).strip()
    if actual != baseline['commit']:
        raise ValueError('official tag differs from the reviewed upstream commit')
    print('UPSTREAM_BASELINE_VERIFIED ' + actual)


if __name__ == '__main__':
    main()
