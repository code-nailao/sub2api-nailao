#!/usr/bin/env python3
"""Publish fixed images once; resume only a verified original bundle and receipt."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sources_baseline import load_identity
from verify_bundle import sha256, verify
from build_context import create_context

IDENTITY = load_identity()
REPOSITORY = IDENTITY['repository']
REPOSITORIES = dict(IDENTITY['images'])


def decision(actual, recorded):
    if recorded:
        if actual != recorded['digest']:
            raise ValueError('published digest missing or changed; never overwrite')
        return 'reuse'
    if actual:
        raise ValueError('tag exists without its original receipt; reconcile, never overwrite')
    return 'build'


def inspect(ref):
    result = subprocess.run(['docker','buildx','imagetools','inspect',ref], capture_output=True, text=True, timeout=90)
    if result.returncode:
        if re.search(r'manifest unknown|name unknown|not found', result.stderr, re.I):
            return None
        raise RuntimeError('registry inspection failed; absence is unverified: '+result.stderr[-1500:])
    match = re.search(r'^Digest:\s+(sha256:[a-f0-9]{64})\s*$', result.stdout, re.M)
    if not match:
        raise ValueError('registry did not return an immutable manifest digest')
    return match.group(1)


def save(path, value):
    temp = path.with_suffix('.tmp')
    with temp.open('w') as f:
        json.dump(value, f, indent=2); f.write('\n'); f.flush(); os.fsync(f.fileno())
    temp.replace(path)


def initial_receipt(manifest, manifest_sha, ci):
    # Only a completed, successful native push CI run on this exact source
    # backs publication. Local or substitute validation receipts are refused.
    if (not isinstance(ci, dict) or 'mode' in ci
            or ci.get('repository') != REPOSITORY or ci.get('head_sha') != manifest['source_commit']
            or ci.get('event') != 'push' or ci.get('conclusion') != 'success' or ci.get('status') != 'completed'):
        raise ValueError('publication requires a successful exact-source native push CI receipt')
    return {'schema':1, 'source_commit':manifest['source_commit'], 'version':manifest['version'],
            'build_manifest_sha256':manifest_sha, 'ci':ci, 'images':{},
            'cutover':False, 'latest_promoted':False, 'studio_manifest_required':True}


def validate_resume(original, current):
    for key in ('schema','source_commit','version','build_manifest_sha256'):
        if original[key] != current[key]:
            raise ValueError('resume identity differs: '+key)
    if set(original['images'])-set(REPOSITORIES):
        raise ValueError('unexpected component in receipt')
    for component, item in original['images'].items():
        if (item['repository'] != REPOSITORIES[component] or item['tag'] != 'v'+current['version']
            or item['source_commit'] != current['source_commit'] or item['platform'] != 'linux/amd64'
            or not re.fullmatch('sha256:[a-f0-9]{64}', item['digest'])):
            raise ValueError('invalid component receipt')
    return original['images']


def confirm_image(path, receipt, component, image, inspect_image=inspect):
    # Durable before remote confirmation: a network failure after a successful
    # push must retain the builder's digest and resume without another build.
    receipt['images'][component] = dict(image, registry_verified=False)
    save(path,receipt)
    if inspect_image(image['repository']+':'+image['tag']) != image['digest']:
        raise ValueError('published registry digest does not match builder receipt')
    receipt['images'][component]['registry_verified'] = True
    save(path,receipt)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('bundle'); p.add_argument('--output',required=True)
    p.add_argument('--sha',required=True); p.add_argument('--version',required=True)
    p.add_argument('--ci',required=True); p.add_argument('--resume')
    a = p.parse_args()
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+',a.version):
        raise ValueError('invalid fixed version')
    root = Path(a.bundle).resolve(); out = Path(a.output).resolve()
    if out.is_relative_to(root): raise ValueError('publication output must not alter verified inputs')
    m = verify(root,a.sha)
    if m['version'] != a.version: raise ValueError('VERSION differs from original bundle')
    ci = json.loads(Path(a.ci).read_text())
    receipt = initial_receipt(m,sha256(root/'build-manifest.json'),ci)
    if a.resume:
        receipt['images'] = validate_resume(json.loads(Path(a.resume).read_text()),receipt)
        receipt['resumed_from_sha256'] = sha256(a.resume)
    out.mkdir(parents=True,exist_ok=False)
    save(out/'published-images.json',receipt)
    # Preflight all tags before performing the first push.
    actions = {component:decision(inspect(repo+':v'+a.version),receipt['images'].get(component))
               for component,repo in REPOSITORIES.items()}
    for component,repo in REPOSITORIES.items():
        if actions[component] == 'reuse':
            receipt['images'][component]['registry_verified'] = True
            save(out/'published-images.json',receipt)
            print('IMAGE_REUSED '+component+' '+receipt['images'][component]['digest'],flush=True)
            continue
        metadata = out/(component+'-image-metadata.json')
        context = out/(component+'-context.tar')
        create_context(root/component,context)
        command = ['docker','buildx','build','--platform','linux/amd64','--provenance=mode=max','--sbom=true','--push',
                   '--build-arg','SOURCE_COMMIT='+a.sha,'--build-arg','VERSION='+a.version,
                   '--build-arg','BASE_IMAGE='+m['upstream']['image'],
                   '--build-arg','SOURCE_DATE_EPOCH='+str(m['source_date_epoch']),
                   '--label','org.opencontainers.image.version='+a.version,
                   '--label','org.opencontainers.image.revision='+a.sha,
                   '--label','org.opencontainers.image.source=https://github.com/'+REPOSITORY,
                   '--label','com.sources.build-manifest-sha256='+receipt['build_manifest_sha256'],
                   '--tag',repo+':v'+a.version,'--metadata-file',str(metadata),'-']
        with context.open('rb') as stream:
            subprocess.run(command,stdin=stream,check=True,timeout=1800)
        digest = json.loads(metadata.read_text())['containerimage.digest']
        if not re.fullmatch('sha256:[a-f0-9]{64}',digest):
            raise ValueError('builder metadata has no valid digest')
        image = {'repository':repo,'tag':'v'+a.version,'digest':digest,
                 'source_commit':a.sha,'platform':'linux/amd64'}
        confirm_image(out/'published-images.json',receipt,component,image)
        print('IMAGE_PUBLISHED '+component+' '+digest,flush=True)
    print('CORE_PUBLICATION_COMPLETE no_cutover=true studio_manifest_required=true')


if __name__ == '__main__': main()
