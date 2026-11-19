"""Current reviewed upstream identity shared by validation and release tools.

Historical evidence is verified with its archived source/harness. It must never
be rewritten to claim that it exercised this newer baseline.
"""
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]


def load_baseline(root=ROOT):
    value = json.loads((Path(root) / '.upstream-sub2api').read_text())
    if (value.get('schema') != 1
            or value.get('repository') != 'https://github.com/Wei-Shaw/sub2api'
            or not re.fullmatch(r'v\d+\.\d+\.\d+', value.get('version', ''))
            or not re.fullmatch(r'[a-f0-9]{40}', value.get('commit', ''))
            or not re.fullmatch(r'ghcr.io/wei-shaw/sub2api@sha256:[a-f0-9]{64}', value.get('image', ''))
            or value.get('platform') != 'linux/amd64'
            or type(value.get('migration_count')) is not int
            or value['migration_count'] <= 0):
        raise ValueError('invalid pinned upstream identity')
    return value


def load_identity(root=ROOT):
    """Return this repository's own GitHub name and image repositories.

    Release tools and the release workflow read the publication identity from
    `.sources-identity.json` instead of hardcoding it, so a renamed or forked
    repository fails closed rather than publishing under another name.
    """
    value = json.loads((Path(root) / '.sources-identity.json').read_text())
    if not isinstance(value, dict) or set(value) != {'schema', 'repository', 'images'}:
        raise ValueError('invalid repository identity')
    images = value['images']
    if (value['schema'] != 1
            or not isinstance(value.get('repository'), str)
            or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', value['repository'])
            or not isinstance(images, dict)
            or set(images) != {'main', 'portal'}
            or not all(isinstance(ref, str) and re.fullmatch(r'ghcr\.io/[a-z0-9_.-]+/[a-z0-9_.-]+', ref)
                       for ref in images.values())):
        raise ValueError('invalid repository identity')
    return value
