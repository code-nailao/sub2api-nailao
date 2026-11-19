import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from release_assets import stable_images, ensure_assets


class ReleaseAssetTests(unittest.TestCase):
    def test_partial_release_uploads_only_missing_assets(self):
        with tempfile.TemporaryDirectory() as tmp:
            files=[]
            for name in ['build-manifest.json','release-images.json','core-source.tar.gz']:
                file=Path(tmp)/name;file.write_text(name);files.append(file)
            uploaded=[]
            ensure_assets('v0.2.27',files,{files[0].name},lambda tag,name:Path(tmp)/name,
                          lambda tag,file:uploaded.append(file.name))
            self.assertEqual(uploaded,[files[1].name,files[2].name])
    def test_different_existing_asset_prevents_all_uploads(self):
        with tempfile.TemporaryDirectory() as tmp:
            a=Path(tmp)/'a';a.write_text('expected');b=Path(tmp)/'b';b.write_text('changed')
            uploaded=[]
            with self.assertRaises(ValueError):ensure_assets('v1',[a,b],{'b'},lambda tag,name:a,lambda *args:uploaded.append(args))
            self.assertEqual(uploaded,[])
    def test_attempt_metadata_does_not_change_release_asset(self):
        image={'repository':'fixture','tag':'v0.2.27','digest':'sha256:'+'c'*64,'source_commit':'a'*40,'platform':'linux/amd64','registry_verified':True}
        receipt={'source_commit':'a'*40,'version':'0.2.27','build_manifest_sha256':'b'*64,'images':{'main':image,'portal':image},'ci':{'id':1}}
        resumed=copy.deepcopy(receipt);resumed['ci']['id']=2;resumed['resumed_from_sha256']='d'*64
        self.assertEqual(json.dumps(stable_images(receipt),sort_keys=True),json.dumps(stable_images(resumed),sort_keys=True))


if __name__=='__main__':unittest.main()
