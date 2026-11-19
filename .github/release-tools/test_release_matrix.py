import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('release_matrix', Path(__file__).with_name('release_matrix.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseMatrixTest(unittest.TestCase):
    def setUp(self):
        self.previous = Path.cwd()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, self.previous)
        for name in ('.goreleaser.yaml', '.goreleaser.simple.yaml'):
            shutil.copyfile(ROOT / name, name)
        Path('backend/cmd/server').mkdir(parents=True)
        release.VERSION_FILE.write_text('9.8.7\n')

    def fixture_artifacts(self, simple=False):
        directory = Path('release-input')
        directory.mkdir()
        for target in release.targets(simple):
            name = release.archive_name('9.8.7', target)
            archive = directory / name
            if target['goos'] == 'linux':
                with tarfile.open(archive, 'w:gz') as out:
                    info = tarfile.TarInfo('sub2api')
                    info.size = 7
                    info.mode = 0o755
                    out.addfile(info, io.BytesIO(b'fixture'))
            else:
                archive.write_bytes(b'fixture archive')
            metadata = {'version': '9.8.7', 'sha': 'a' * 40, 'target': target,
                        'archive': name, 'sha256': release.sha256(archive)}
            (directory / f"manifest-{target['goos']}-{target['goarch']}.json").write_text(json.dumps(metadata))
        return argparse.Namespace(input='release-input', version='9.8.7', sha='a' * 40, simple=simple, output='contexts')

    def test_full_and_simple_matrix_match_existing_targets(self):
        full = release.targets()
        self.assertEqual(len(full), 5)
        self.assertNotIn({'goos': 'windows', 'goarch': 'arm64'}, full)
        self.assertEqual(release.targets(True), [{'goos': 'linux', 'goarch': 'amd64'}])

    def test_leaf_keeps_packaging_and_selects_only_one_target(self):
        original = release.config()
        release.generate_config(argparse.Namespace(mode='build', simple=False, goos='darwin', goarch='arm64', output='leaf.yaml'))
        leaf = yaml.safe_load(Path('leaf.yaml').read_text())
        self.assertEqual(leaf['builds'][0]['goos'], ['darwin'])
        self.assertEqual(leaf['builds'][0]['goarch'], ['arm64'])
        self.assertEqual(leaf['builds'][0]['ignore'], [])
        self.assertEqual(leaf['archives'], original['archives'])
        self.assertEqual(leaf['release'], original['release'])
        self.assertFalse(leaf['dockers'])
        self.assertIn('{{ .Env.RELEASE_DATE }}', '\n'.join(leaf['builds'][0]['ldflags']))

    def test_publication_config_has_no_compilation_or_docker_work(self):
        for simple in (False, True):
            with self.subTest(simple=simple):
                original = release.config(simple)
                release.generate_config(argparse.Namespace(mode='publish', simple=simple, output='publisher.yaml'))
                data = yaml.safe_load(Path('publisher.yaml').read_text())
                self.assertTrue(data['builds'][0]['skip'])
                self.assertFalse(data['archives'])
                self.assertFalse(data['dockers'])
                self.assertEqual(data['release']['header'], original['release']['header'])
                self.assertEqual(data['release']['footer'], original['release']['footer'])
                if simple:
                    self.assertTrue(data['checksum']['disable'])
                    self.assertTrue(data['release']['skip_upload'])
                else:
                    self.assertEqual(data['checksum']['extra_files'], data['release']['extra_files'])

    def test_collect_and_verify_hash_and_source_binding(self):
        args = self.fixture_artifacts()
        release.verify(args)
        file = next(Path(args.input).glob('*.tar.gz'))
        file.write_bytes(b'corrupted')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            release.verify(args)

    def test_missing_extra_and_wrong_commit_artifacts_are_rejected(self):
        args = self.fixture_artifacts(True)
        args.sha = 'b' * 40
        with self.assertRaises(ValueError):
            release.verify(args)
        args.sha = 'a' * 40
        Path('release-input/unexpected').write_text('not an asset')
        with self.assertRaises(ValueError):
            release.verify(args)
        Path('release-input/unexpected').unlink()
        next(Path('release-input').glob('*.tar.gz')).unlink()
        with self.assertRaises(FileNotFoundError):
            release.verify(args)

    def test_linux_context_preserves_binary_executable_mode(self):
        args = self.fixture_artifacts()
        Path('Dockerfile.goreleaser').write_text('FROM scratch\nCOPY sub2api /sub2api\n')
        Path('deploy').mkdir()
        Path('deploy/docker-entrypoint.sh').write_text('#!/bin/sh\nexec /app/sub2api\n')
        Path('backend/resources').mkdir()
        Path('backend/resources/data').write_text('fixture')
        release.contexts(args)
        for arch in ('amd64', 'arm64'):
            binary = Path('contexts') / arch / 'sub2api'
            self.assertEqual(binary.read_bytes(), b'fixture')
            self.assertEqual(binary.stat().st_mode & 0o777, 0o755)

    def test_plan_requires_a_tag_for_publication(self):
        args = argparse.Namespace(ref='main', dry_run=False, simple=False)
        with patch.object(subprocess, 'check_output', return_value='a' * 40 + '\n'):
            with self.assertRaisesRegex(ValueError, 'version tag'):
                release.plan(args)
        args.ref = 'v9.8.7'
        with patch.object(subprocess, 'check_output', side_effect=['a' * 40 + '\n', 'b' * 40 + '\n']):
            with self.assertRaisesRegex(ValueError, 'does not match'):
                release.plan(args)

    def test_dry_run_plan_resolves_matrix_without_a_new_tag(self):
        with patch.dict(os.environ, {'GITHUB_OUTPUT': 'outputs', 'GITHUB_REPOSITORY_OWNER': 'ExampleOwner'}), patch.object(subprocess, 'check_output', return_value='a' * 40 + '\n'):
            release.plan(argparse.Namespace(ref='feature/matrix', dry_run=True, simple=False))
        output = dict(line.split('=', 1) for line in Path('outputs').read_text().splitlines())
        self.assertEqual(output['dry_run'], 'true')
        self.assertEqual(output['owner_lower'], 'exampleowner')
        self.assertEqual(len(json.loads(output['matrix'])['include']), 5)

    def test_docker_commands_do_not_publish_during_dry_run(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
               'DOCKER_LOG': str(Path('docker.log').resolve()), 'RUNNER_TEMP': self.temp.name,
               'RELEASE_VERSION': '9.8.7', 'RELEASE_SHA': 'a' * 40, 'GITHUB_REPOSITORY': 'ExampleOwner/sub2api',
               'DRY_RUN': 'true', 'SIMPLE_RELEASE': 'false', 'DOCKERHUB_USERNAME': 'skip'}
        subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
        log = Path('docker.log').read_text()
        self.assertEqual(log.count('buildx build'), 2)
        self.assertIn('linux/arm64', log)
        self.assertNotIn('--push', log)
        self.assertNotIn('imagetools', log)
        self.assertNotIn('skip/sub2api', log)
        self.assertIn('ghcr.io/exampleowner/sub2api', log)


    def test_published_full_and_simple_image_tags(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        for simple in (False, True):
            with self.subTest(simple=simple):
                log_path = Path(f'docker-{simple}.log').resolve()
                env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
                       'DOCKER_LOG': str(log_path), 'RUNNER_TEMP': self.temp.name,
                       'RELEASE_VERSION': '9.8.7', 'RELEASE_SHA': 'a' * 40, 'GITHUB_REPOSITORY': 'ExampleOwner/sub2api',
                       'DRY_RUN': 'false', 'SIMPLE_RELEASE': str(simple).lower(), 'DOCKERHUB_USERNAME': 'fixturehub'}
                subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
                log = log_path.read_text()
                self.assertIn('--push', log)
                self.assertEqual(log.count('buildx build'), 1 if simple else 2)
                if simple:
                    self.assertNotIn('fixturehub', log)
                    self.assertNotIn('imagetools', log)
                    self.assertIn('ghcr.io/exampleowner/sub2api:latest', log)
                else:
                    self.assertEqual(log.count('imagetools create'), 2)
                    self.assertIn('fixturehub/sub2api:9.8', log)
                    self.assertIn('ghcr.io/exampleowner/sub2api:9', log)



scope_spec = importlib.util.spec_from_file_location('clean_components', Path(__file__).with_name('clean_components.py'))
scoped = importlib.util.module_from_spec(scope_spec)
scope_spec.loader.exec_module(scoped)


class CleanComponentScopeTests(unittest.TestCase):
    def fixture(self, root, version='0.2.62'):
        root = Path(root)
        bundle = root / 'bundle'
        bundle.mkdir()
        files = {'main/sub2api': b'fixture-main', 'main/Dockerfile': b'FROM scratch\nCOPY sub2api /app/sub2api\n',
                 'portal/sources-portal': b'fixture-portal-not-published', 'portal/Dockerfile': b'FROM scratch\n',
                 'portal/homeweb/index.html': b'fixture-html', 'core-source.tar.gz': b'fixture-source'}
        for name, data in files.items():
            path = bundle / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        baseline = json.loads((ROOT / '.upstream-sub2api').read_text())
        manifest = {'source_commit': 'a' * 40, 'version': version, 'platform': baseline['platform'],
                    'upstream_commit': baseline['commit'], 'upstream': baseline, 'source_date_epoch': 123,
                    'files': {name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
        (bundle / 'build-manifest.json').write_text(json.dumps(manifest))
        ci = root / 'ci.json'
        ci.write_text(json.dumps({'repository': scoped.IDENTITY['repository'], 'head_sha': 'a' * 40,
                                 'event': 'push', 'conclusion': 'success', 'status': 'completed'}))
        return argparse.Namespace(bundle=str(bundle), output=str(root / 'publication'), sha='a' * 40,
                                  version=version, ci=str(ci), components='auto', resume=None)

    def publish_fixture(self, args):
        publisher = scoped.native_module('publish')
        inspected, built, published = [], [], {}
        def inspect(ref):
            inspected.append(ref)
            return published.get(ref)
        def run(command, **kwargs):
            self.assertEqual(command[:3], ['docker', 'buildx', 'build'])
            tag = command[command.index('--tag') + 1]
            metadata = Path(command[command.index('--metadata-file') + 1])
            digest = 'sha256:' + ('c' * 64 if not tag.startswith(scoped.REPOSITORIES['portal'] + ':') else 'd' * 64)
            metadata.write_text(json.dumps({'containerimage.digest': digest}))
            published[tag] = digest
            built.append(command)
            return subprocess.CompletedProcess(command, 0)
        original_confirm = publisher.confirm_image
        def confirm(path, receipt, component, image):
            return original_confirm(path, receipt, component, image, inspect_image=inspect)
        with patch.object(publisher, 'inspect', side_effect=inspect), patch.object(publisher, 'confirm_image', side_effect=confirm), patch.object(publisher.subprocess, 'run', side_effect=run):
            scoped.publish_images(args, publisher=publisher)
        return json.loads((Path(args.output) / 'published-images.json').read_text()), inspected, built

    def test_062_tag_automatically_publishes_only_main(self):
        self.assertEqual(scoped.selected_components('0.2.62'), ('main',))
        with tempfile.TemporaryDirectory() as tmp:
            receipt, inspected, built = self.publish_fixture(self.fixture(tmp))
            self.assertEqual(receipt['publication_scope'], ['main'])
            self.assertEqual(set(receipt['images']), {'main'})
            self.assertEqual(len(built), 1)
            self.assertTrue(inspected)
            self.assertTrue(all(ref == scoped.REPOSITORIES['main'] + ':v0.2.62' for ref in inspected))
            self.assertNotIn(scoped.REPOSITORIES['portal'], ' '.join(inspected))
            self.assertEqual(receipt['images']['main']['tag'], 'v0.2.62')
            self.assertTrue(receipt['images']['main']['registry_verified'])
            self.assertEqual(receipt['component_selection']['source_commit'], 'a' * 40)

    def test_unrelated_release_still_publishes_both_components(self):
        self.assertEqual(scoped.selected_components('9.8.7'), ('main', 'portal'))
        with tempfile.TemporaryDirectory() as tmp:
            receipt, inspected, built = self.publish_fixture(self.fixture(tmp, '9.8.7'))
            self.assertEqual(set(receipt['images']), {'main', 'portal'})
            self.assertEqual(len(built), 2)
            self.assertIn(scoped.REPOSITORIES['portal'] + ':v9.8.7', inspected)
            self.assertNotIn('publication_scope', scoped.stable_images(receipt, scoped.DEFAULT))

    def test_manual_main_scope_for_unpinned_release(self):
        self.assertEqual(scoped.selected_components('9.8.7', 'main'), ('main',))

    def test_062_cannot_override_to_both(self):
        with self.assertRaisesRegex(ValueError, 'conflict'):
            scoped.selected_components('0.2.62', 'both')

    def test_unsupported_and_empty_scope_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.fixture(tmp, '9.8.7')
            for value in ('', 'portal', 'main,main', 'unknown', 'false'):
                args.components = value
                with self.subTest(value=value), patch.object(scoped, 'native_module', side_effect=AssertionError('must not load publisher')):
                    with self.assertRaises(ValueError):
                        scoped.publish_images(args)
                self.assertFalse(Path(args.output).exists())

    def test_manifest_cannot_change_default_for_every_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / 'manifest.json'
            p.write_text(json.dumps({'schema': 1, 'default': ['main'], 'releases': {}}))
            with self.assertRaisesRegex(ValueError, 'unrelated releases'):
                scoped.load_scope_manifest(p)

    def test_manifest_duplicate_or_unknown_components_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / 'manifest.json'
            for components in ([], ['main', 'main'], ['portal'], ['main', 'studio']):
                p.write_text(json.dumps({'schema': 1, 'default': ['main', 'portal'], 'releases': {'0.2.62': components}}))
                with self.subTest(components=components), self.assertRaises(ValueError):
                    scoped.load_scope_manifest(p)

    def test_resume_cannot_expand_or_narrow_original_scope(self):
        original = {'version': '9.8.7', 'source_commit': 'a' * 40, 'publication_scope': ['main'], 'images': {}}
        with self.assertRaisesRegex(ValueError, 'scope changed'):
            scoped.validate_receipt_scope(original, ('main', 'portal'))
        original.pop('publication_scope')
        self.assertEqual(scoped.validate_receipt_scope(original, ('main', 'portal')), ('main', 'portal'))
        with self.assertRaisesRegex(ValueError, 'scope changed'):
            scoped.validate_receipt_scope(original, ('main',))

    def test_resume_rejects_scope_change_before_any_registry_call(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.fixture(tmp)
            original = Path(tmp) / 'resume.json'
            original.write_text(json.dumps({'version': args.version, 'source_commit': args.sha, 'publication_scope': ['main', 'portal'], 'images': {}}))
            args.resume = str(original)
            with patch.object(scoped, 'native_module', side_effect=AssertionError('must not load publisher')):
                with self.assertRaisesRegex(ValueError, 'scope changed'):
                    scoped.publish_images(args)

    def test_wrong_component_tag_source_and_unverified_digest_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            receipt, _, _ = self.publish_fixture(self.fixture(tmp))
            for field, value in (('repository', scoped.REPOSITORIES['portal']), ('tag', 'v0.2.61'),
                                 ('source_commit', 'b' * 40), ('registry_verified', False), ('digest', 'latest')):
                changed = json.loads(json.dumps(receipt))
                changed['images']['main'][field] = value
                with self.subTest(field=field), self.assertRaises(ValueError):
                    scoped.stable_images(changed, ('main',))

    def test_main_only_asset_metadata_accepts_no_portal_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            receipt, _, _ = self.publish_fixture(self.fixture(tmp))
            stable = scoped.stable_images(receipt, ('main',))
            self.assertEqual(stable['publication_scope'], ['main'])
            self.assertFalse(stable['cutover'])
            self.assertFalse(stable['latest_promoted'])
            receipt['images']['portal'] = dict(receipt['images']['main'])
            with self.assertRaises(ValueError):
                scoped.stable_images(receipt, ('main',))

    def test_assets_use_native_immutable_upload_and_full_readback(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.fixture(tmp)
            receipt, _, _ = self.publish_fixture(args)
            asset = scoped.native_module('release_assets')
            remote, commands = {}, []
            def api(path, optional=False):
                if path.startswith('git/ref/tags/'):
                    return {'object': {'type': 'commit', 'sha': args.sha}}
                if path == 'releases/latest':
                    return None
                if path.startswith('releases/tags/'):
                    return {'tag_name': 'v' + args.version, 'draft': False, 'assets': []}
                raise AssertionError(path)
            def run(command, **kwargs):
                commands.append(command)
                self.assertEqual(command[:2], ['gh', 'release'])
                if command[2] == 'upload':
                    self.assertNotIn('--clobber', command)
                    file = Path(command[4]); remote[file.name] = file.read_bytes()
                elif command[2] == 'download':
                    name = command[command.index('--pattern') + 1]
                    target = Path(command[command.index('--dir') + 1]) / name
                    target.write_bytes(remote[name])
                else:
                    raise AssertionError(command)
                return subprocess.CompletedProcess(command, 0)
            notes = Path(tmp) / 'notes.md'; notes.write_text('Main only; Portal remains unchanged.')
            asset_args = argparse.Namespace(bundle=args.bundle, publication=args.output, notes=str(notes), components='main')
            with patch.object(asset, 'api', side_effect=api), patch.object(asset.subprocess, 'run', side_effect=run):
                scoped.publish_assets(asset_args, asset_publisher=asset)
            self.assertEqual(set(remote), {'build-manifest.json', 'release-images.json', 'core-source.tar.gz'})
            self.assertEqual(len([cmd for cmd in commands if cmd[2] == 'download']), 3)
            self.assertEqual(set(json.loads(remote['release-images.json'])['images']), {'main'})

    def test_changed_asset_bundle_fails_before_github(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.fixture(tmp)
            self.publish_fixture(args)
            (Path(args.bundle) / 'main/sub2api').write_bytes(b'changed')
            assets = scoped.native_module('release_assets')
            with patch.object(assets, 'main', side_effect=AssertionError('must not publish')):
                with self.assertRaisesRegex(ValueError, 'bytes changed'):
                    scoped.publish_assets(argparse.Namespace(bundle=args.bundle, publication=args.output, notes='unused', components='main'), asset_publisher=assets)

    def test_workflow_threads_scope_to_images_and_assets(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/release.yml').read_text())
        trigger = workflow.get('on', workflow.get(True))
        self.assertEqual(trigger['workflow_dispatch']['inputs']['components']['default'], 'auto')
        self.assertEqual(workflow['jobs']['build']['outputs']['components'], '${{ steps.identity.outputs.components }}')
        build = next(step for step in workflow['jobs']['build']['steps'] if step.get('id') == 'identity')
        self.assertIn('clean_components.py resolve', build['run'])
        steps = workflow['jobs']['publish']['steps']
        images = next(step for step in steps if 'clean_components.py images' in step.get('run', ''))
        assets = next(step for step in steps if 'clean_components.py assets' in step.get('run', ''))
        for step in (images, assets):
            self.assertEqual(step['env']['PUBLISH_COMPONENTS'], '${{ needs.build.outputs.components }}')
            self.assertIn('--components "$PUBLISH_COMPONENTS"', step['run'])
            self.assertIn('[[ "$PUBLISH_COMPONENTS" == main,portal ]]', step['run'])
        self.assertIn('Publish fixed Main image', images['name'])
        for job in workflow['jobs'].values():
            for step in job['steps']:
                if 'run' in step:
                    subprocess.run(['bash', '-n'], input=step['run'], text=True, check=True, capture_output=True)


if __name__ == '__main__':
    unittest.main()
