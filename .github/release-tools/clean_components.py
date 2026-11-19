#!/usr/bin/env python3
"""Select a fixed clean-release publication scope, preserving native publishers.
Build bundles may contain both components; only selected images are inspected,
published and required for release metadata. Existing images/assets remain immutable.
"""
import argparse
from contextlib import contextmanager
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[2]
MANIFEST = Path(__file__).with_name('component-scopes.json')
DEFAULT = ('main', 'portal')
if str(ROOT / 'scripts') not in sys.path:
    sys.path.insert(0, str(ROOT / 'scripts'))
from sources_baseline import load_identity  # noqa: E402

IDENTITY = load_identity(ROOT)
REPOSITORIES = {name: IDENTITY['images'][name] for name in DEFAULT}
VALID_SCOPES = (('main',), DEFAULT)


def load_scope_manifest(path=MANIFEST):
    value = json.loads(Path(path).read_text())
    if set(value) != {'schema', 'default', 'releases'} or type(value['schema']) is not int or value['schema'] != 1:
        raise ValueError('invalid component-scope manifest')
    if value['default'] != list(DEFAULT) or not isinstance(value['releases'], dict):
        raise ValueError('unrelated releases must retain the default Main and Portal scope')
    for version, components in value['releases'].items():
        if not re.fullmatch(r'\d+\.\d+\.\d+', version) or not isinstance(components, list) or tuple(components) not in VALID_SCOPES:
            raise ValueError('invalid pinned release component scope')
    return value


def selected_components(version, requested='auto', *, manifest_path=MANIFEST):
    if not re.fullmatch(r'\d+\.\d+\.\d+', version):
        raise ValueError('invalid clean release version')
    manifest = load_scope_manifest(manifest_path)
    pinned = manifest['releases'].get(version)
    if requested == 'auto':
        return tuple(pinned or manifest['default'])
    choices = {'main': ('main',), 'both': DEFAULT, 'main,portal': DEFAULT}
    if requested not in choices:
        raise ValueError('components must be auto, main or both')
    selected = choices[requested]
    if pinned is not None and selected != tuple(pinned):
        raise ValueError('requested components conflict with the source-pinned release scope')
    return selected


def selection_receipt(version, source, components, *, manifest_path=MANIFEST):
    if not re.fullmatch(r'[a-f0-9]{40}', source):
        raise ValueError('full source commit required')
    if tuple(components) != selected_components(version, ','.join(components), manifest_path=manifest_path):
        raise ValueError('invalid component selection')
    return {'schema': 1, 'version': version, 'source_commit': source, 'components': list(components),
            'manifest_sha256': hashlib.sha256(Path(manifest_path).read_bytes()).hexdigest()}


def validate_receipt_scope(receipt, selected, *, complete=False):
    selected = tuple(selected)
    if selected not in VALID_SCOPES:
        raise ValueError('invalid component selection')
    # Receipts predating component selection always represented the original
    # two-component publication, unless they explicitly recorded Main-only.
    recorded = receipt.get('publication_scope', list(DEFAULT))
    if not isinstance(recorded, list) or tuple(recorded) != selected:
        raise ValueError('publication scope changed; reconcile instead of expanding/narrowing a retry')
    actual = set(receipt['images'])
    if actual - set(selected) or complete and actual != set(selected):
        raise ValueError('published component set differs from selected scope')
    selection = receipt.get('component_selection')
    if selection is not None:
        if (selection.get('schema') != 1 or selection.get('version') != receipt['version']
                or selection.get('source_commit') != receipt['source_commit']
                or selection.get('components') != list(selected)):
            raise ValueError('component-selection receipt identity differs')
    return selected


def stable_images(receipt, selected):
    selected = validate_receipt_scope(receipt, selected, complete=True)
    fields = ('repository', 'tag', 'digest', 'source_commit', 'platform')
    images = {}
    for component in selected:
        item = receipt['images'][component]
        if (item.get('registry_verified') is not True or item['repository'] != REPOSITORIES[component]
                or item['tag'] != 'v' + receipt['version'] or item['source_commit'] != receipt['source_commit']
                or item['platform'] != 'linux/amd64' or not re.fullmatch(r'sha256:[a-f0-9]{64}', item['digest'])):
            raise ValueError('component image tag/source/digest verification incomplete')
        images[component] = {key: item[key] for key in fields}
    value = {'schema': 1, 'source_commit': receipt['source_commit'], 'version': receipt['version'],
             'build_manifest_sha256': receipt['build_manifest_sha256'], 'images': images,
             'cutover': False, 'latest_promoted': False, 'studio_manifest_required': True}
    # Keep the normal two-component release-images.json wire shape unchanged.
    if selected != DEFAULT:
        value['publication_scope'] = list(selected)
    return value


def native_module(name):
    directory = ROOT / 'scripts/sources_release'
    if str(directory) not in sys.path:
        sys.path.insert(0, str(directory))
    spec = importlib.util.spec_from_file_location('_scoped_native_' + name, directory / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@contextmanager
def arguments(argv):
    previous = sys.argv
    sys.argv = argv
    try:
        yield
    finally:
        sys.argv = previous


def publish_images(args, *, publisher=None, manifest_path=MANIFEST):
    selected = selected_components(args.version, args.components, manifest_path=manifest_path)
    selection = selection_receipt(args.version, args.sha, selected, manifest_path=manifest_path)
    if args.resume:
        original = json.loads(Path(args.resume).read_text())
        validate_receipt_scope(original, selected)
        if original.get('component_selection') not in (None, selection):
            raise ValueError('source-pinned publication selection changed since original attempt')
    publisher = publisher or native_module('publish')
    original_repositories, original_initial = publisher.REPOSITORIES, publisher.initial_receipt

    def initial(*values):
        receipt = original_initial(*values)
        if receipt['version'] != args.version or receipt['source_commit'] != args.sha:
            raise ValueError('native bundle identity differs from component selection')
        receipt['publication_scope'] = list(selected)
        receipt['component_selection'] = selection
        return receipt

    publisher.REPOSITORIES = {name: REPOSITORIES[name] for name in selected}
    publisher.initial_receipt = initial
    command = ['publish.py', args.bundle, '--output', args.output, '--sha', args.sha,
               '--version', args.version, '--ci', args.ci]
    if args.resume:
        command += ['--resume', args.resume]
    try:
        with arguments(command):
            publisher.main()
    finally:
        publisher.REPOSITORIES, publisher.initial_receipt = original_repositories, original_initial
    receipt = json.loads((Path(args.output) / 'published-images.json').read_text())
    stable_images(receipt, selected)
    print('SELECTED_IMAGE_PUBLICATION_VERIFIED components=' + ','.join(selected) + ' cutover=false')


def publish_assets(args, *, asset_publisher=None, manifest_path=MANIFEST):
    receipt = json.loads((Path(args.publication) / 'published-images.json').read_text())
    selected = selected_components(receipt['version'], args.components, manifest_path=manifest_path)
    selection = selection_receipt(receipt['version'], receipt['source_commit'], selected, manifest_path=manifest_path)
    if receipt.get('component_selection') not in (None, selection):
        raise ValueError('source-pinned scope differs from the image publication')
    stable_images(receipt, selected)
    verifier = native_module('verify_bundle')
    manifest = verifier.verify(args.bundle, receipt['source_commit'])
    if (manifest['version'] != receipt['version'] or
            verifier.sha256(Path(args.bundle) / 'build-manifest.json') != receipt['build_manifest_sha256']):
        raise ValueError('release assets differ from the published build bundle')
    asset_publisher = asset_publisher or native_module('release_assets')
    original = asset_publisher.stable_images
    asset_publisher.stable_images = lambda value: stable_images(value, selected)
    try:
        with arguments(['release_assets.py', '--bundle', args.bundle, '--publication', args.publication, '--notes', args.notes]):
            asset_publisher.main()
    finally:
        asset_publisher.stable_images = original


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    resolve = commands.add_parser('resolve')
    resolve.add_argument('--version', required=True)
    resolve.add_argument('--requested', default='auto', choices=('auto', 'main', 'both'))
    images = commands.add_parser('images')
    for key in ('bundle', 'output', 'sha', 'version', 'ci', 'components'):
        images.add_argument('--' + key, required=True)
    images.add_argument('--resume')
    assets = commands.add_parser('assets')
    for key in ('bundle', 'publication', 'notes', 'components'):
        assets.add_argument('--' + key, required=True)
    args = parser.parse_args()
    if args.command == 'resolve':
        print(','.join(selected_components(args.version, args.requested)))
    elif args.command == 'images':
        publish_images(args)
    else:
        publish_assets(args)


if __name__ == '__main__':
    main()
