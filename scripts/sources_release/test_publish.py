import copy
import json
import tempfile
from pathlib import Path
import unittest
from publish import decision, initial_receipt, validate_resume, confirm_image, REPOSITORY, REPOSITORIES


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.source='a'*40
        self.manifest={'source_commit':self.source,'version':'0.2.27'}
        self.ci={'repository':REPOSITORY,'head_sha':self.source,'event':'push','conclusion':'success','status':'completed'}
        self.receipt=initial_receipt(self.manifest,'b'*64,self.ci)
        self.image={'repository':REPOSITORIES['main'],'tag':'v0.2.27','digest':'sha256:'+'c'*64,'source_commit':self.source,'platform':'linux/amd64'}
    def test_empty_tag_builds(self):self.assertEqual(decision(None,None),'build')
    def test_existing_unreceipted_tag_never_overwritten(self):
        with self.assertRaises(ValueError):decision(self.image['digest'],None)
    def test_receipted_tag_reused(self):self.assertEqual(decision(self.image['digest'],self.image),'reuse')
    def test_missing_published_tag_never_rebuilt(self):
        with self.assertRaises(ValueError):decision(None,self.image)
    def test_changed_tag_rejected(self):
        with self.assertRaises(ValueError):decision('sha256:'+'d'*64,self.image)
    def test_resume_original_bundle(self):
        original=copy.deepcopy(self.receipt);original['images']={'main':self.image}
        self.assertEqual(validate_resume(original,self.receipt),{'main':self.image})
    def test_resume_rebuilt_bundle_rejected(self):
        original=copy.deepcopy(self.receipt);original['build_manifest_sha256']='d'*64
        with self.assertRaises(ValueError):validate_resume(original,self.receipt)
    def test_wrong_component_or_revision_rejected(self):
        original=copy.deepcopy(self.receipt);image=dict(self.image,source_commit='d'*40);original['images']={'main':image}
        with self.assertRaises(ValueError):validate_resume(original,self.receipt)
    def test_failed_ci_rejected(self):
        with self.assertRaises(ValueError):initial_receipt(self.manifest,'b'*64,dict(self.ci,conclusion='failure'))
    def test_push_success_then_registry_failure_keeps_digest_for_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'receipt.json'
            def offline(ref): raise RuntimeError('registry timeout')
            with self.assertRaisesRegex(RuntimeError,'timeout'):
                confirm_image(path,self.receipt,'main',self.image,offline)
            partial=json.loads(path.read_text())
            self.assertFalse(partial['images']['main']['registry_verified'])
            self.assertEqual(decision(self.image['digest'],partial['images']['main']),'reuse')
            confirm_image(path,partial,'main',self.image,lambda ref:self.image['digest'])
            self.assertTrue(json.loads(path.read_text())['images']['main']['registry_verified'])
    def test_other_repository_ci_rejected(self):
        with self.assertRaises(ValueError):initial_receipt(self.manifest,'b'*64,dict(self.ci,repository='other/fork'))
    def test_incomplete_or_other_source_ci_rejected(self):
        for change in ({'status':'in_progress'},{'head_sha':'d'*40}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                initial_receipt(self.manifest,'b'*64,dict(self.ci,**change))
    def test_retired_fallback_receipt_rejected(self):
        # The former local-validation substitute is never a publication receipt,
        # whether in its original shape or padded with native CI fields.
        fallback={'mode':'fallback_billing','repository':REPOSITORY,'head_sha':self.source,
                  'local_result':'passed','stages':['core'],'build_manifest_sha256':'b'*64}
        for receipt in (fallback,dict(fallback,event='push',conclusion='success',status='completed')):
            with self.subTest(keys=sorted(receipt)), self.assertRaises(ValueError):
                initial_receipt(self.manifest,'b'*64,receipt)
    def test_pr_ci_rejected(self):
        with self.assertRaises(ValueError):initial_receipt(self.manifest,'b'*64,dict(self.ci,event='pull_request'))


if __name__=='__main__': unittest.main()
