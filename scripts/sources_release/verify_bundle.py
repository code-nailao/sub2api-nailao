#!/usr/bin/env python3
"""Verify the exact CI input set. Publication receipts belong outside this root."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sources_baseline import load_baseline


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def verify_file_set(root, files, manifest_name):
    root=Path(root)
    paths=list(root.rglob('*'))
    if any(p.is_symlink() for p in paths):raise ValueError('bundle symlink')
    if any(not p.is_dir() and not p.is_file() for p in paths):raise ValueError('non-regular bundle input')
    actual = {p.relative_to(root).as_posix() for p in paths if p.is_file()}
    if actual != set(files) | {manifest_name}:
        raise ValueError('bundle file set differs from manifest')
    for relative, digest in files.items():
        path = PurePosixPath(relative)
        if path.is_absolute() or '..' in path.parts or path.as_posix() != relative:
            raise ValueError('invalid bundle path')
        if not re.fullmatch('[a-f0-9]{64}', digest) or sha256(root/relative) != digest:
            raise ValueError('bundle bytes changed: '+relative)


def verify(root, expected):
    root = Path(root).absolute()
    # macOS /var and /tmp are system symlinks: resolve ancestors, but reject a
    # symlink at the supplied bundle root or anywhere inside the input tree.
    if root.is_symlink():
        raise ValueError('bundle root symlink')
    root = root.resolve()
    paths = list(root.rglob('*'))
    if any(path.is_symlink() for path in paths):
        raise ValueError('bundle symlink')
    if any(not path.is_dir() and not path.is_file() for path in paths):
        raise ValueError('non-regular bundle input')
    m = json.loads((root/'build-manifest.json').read_text())
    if not re.fullmatch('[a-f0-9]{40}', expected) or m['source_commit'] != expected:
        raise ValueError('source identity mismatch')
    baseline = load_baseline()
    if m['platform'] != baseline['platform'] or m['upstream_commit'] != baseline['commit'] or m.get('upstream') != baseline:
        raise ValueError('platform/upstream mismatch')
    required = {'main/sub2api', 'main/Dockerfile', 'portal/sources-portal',
                'portal/Dockerfile', 'portal/homeweb/index.html', 'core-source.tar.gz'}
    if not required.issubset(m['files']):
        raise ValueError('incomplete build bundle')
    verify_file_set(root,m['files'],'build-manifest.json')
    print('BUNDLE_VERIFIED source='+expected+' files='+str(len(m['files'])))
    return m


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory')
    parser.add_argument('--sha', required=True)
    args = parser.parse_args()
    verify(args.directory, args.sha)
