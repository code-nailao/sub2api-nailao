#!/usr/bin/env python3
"""Create a non-latest release, or verify and fill missing immutable assets."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sources_baseline import load_identity
from verify_bundle import sha256

REPO=load_identity()['repository']


def asset_action(actual_hash, expected_hash):
    if actual_hash is None: return 'upload'
    if actual_hash != expected_hash: raise ValueError('existing release asset differs; preserve it')
    return 'reuse'


def stable_images(receipt):
    if set(receipt['images']) != {'main','portal'} or not all(i.get('registry_verified') for i in receipt['images'].values()):
        raise ValueError('publication is incomplete')
    fields=('repository','tag','digest','source_commit','platform')
    return {'schema':1,'source_commit':receipt['source_commit'],'version':receipt['version'],
            'build_manifest_sha256':receipt['build_manifest_sha256'],
            'images':{c:{k:i[k] for k in fields} for c,i in sorted(receipt['images'].items())},
            'cutover':False,'latest_promoted':False,'studio_manifest_required':True}


def api(path, optional=False):
    r=subprocess.run(['gh','api','repos/'+REPO+'/'+path],capture_output=True,text=True)
    if r.returncode:
        if optional and 'HTTP 404' in r.stderr: return None
        raise RuntimeError('GitHub metadata read failed: '+r.stderr[-1500:])
    return json.loads(r.stdout)


def ensure_assets(tag, files, existing, download, upload):
    # Validate ALL already-present assets before uploading any missing file.
    actions=[]
    for file in files:
        original=download(tag,file.name) if file.name in existing else None
        actions.append((file,asset_action(None if original is None else sha256(original),sha256(file))))
    for file,action in actions:
        if action=='upload': upload(tag,file)
        print('RELEASE_ASSET_'+action.upper()+' '+file.name)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--bundle',required=True);p.add_argument('--publication',required=True)
    p.add_argument('--notes',required=True);a=p.parse_args()
    root=Path(a.bundle);out=Path(a.publication)
    receipt=json.loads((out/'published-images.json').read_text())
    stable=stable_images(receipt)
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+',receipt['version']):raise ValueError('invalid version')
    tag='v'+receipt['version']
    ref=api('git/ref/tags/'+tag)['object']
    for _ in range(3):
        if ref['type']!='tag':break
        ref=api('git/tags/'+ref['sha'])['object']
    if ref['type']!='commit' or ref['sha']!=receipt['source_commit']:raise ValueError('tag/source mismatch')
    latest=api('releases/latest',optional=True)
    if latest and latest['tag_name']==tag:raise ValueError('candidate was unexpectedly promoted Latest')
    release=api('releases/tags/'+tag,optional=True)
    if not release:
        subprocess.run(['gh','release','create',tag,'--repo',REPO,'--verify-tag','--latest=false',
                        '--title','Sources '+receipt['version'],'--notes-file',a.notes],check=True)
        release=api('releases/tags/'+tag)
    if release['tag_name']!=tag or release['draft']:raise ValueError('unexpected release identity')
    (out/'release-images.json').write_text(json.dumps(stable,indent=2,sort_keys=True)+'\n')
    files=[root/'build-manifest.json',out/'release-images.json',root/'core-source.tar.gz']
    with tempfile.TemporaryDirectory() as tmp:
        def download(tag,name):
            subprocess.run(['gh','release','download',tag,'--repo',REPO,'--pattern',name,'--dir',tmp],check=True)
            return Path(tmp)/name
        def upload(tag,file):
            # No --clobber: a concurrent/out-of-band asset cannot be overwritten.
            subprocess.run(['gh','release','upload',tag,str(file),'--repo',REPO],check=True)
        ensure_assets(tag,files,{i['name'] for i in release['assets']},download,upload)
    # The asset hashes are independently re-read after upload, not inferred.
    with tempfile.TemporaryDirectory() as tmp:
        for file in files:
            subprocess.run(['gh','release','download',tag,'--repo',REPO,'--pattern',file.name,'--dir',tmp],check=True)
            if sha256(Path(tmp)/file.name)!=sha256(file):raise ValueError('release asset verification failed')
    print('RELEASE_ASSETS_VERIFIED tag='+tag+' latest=false cutover=false')


if __name__=='__main__': main()
