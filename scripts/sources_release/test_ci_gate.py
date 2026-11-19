import unittest
from ci_gate import evaluate, select_run

REPO = 'example-owner/example-repo'
SHA = 'a'*40


def run(id_, **changes):
    value = {'id':id_, 'run_attempt':1, 'event':'push', 'head_sha':SHA,
             'head_repository':{'full_name':REPO}, 'status':'completed', 'conclusion':'success'}
    value.update(changes)
    return value


class CIGateTests(unittest.TestCase):
    def test_pr_merge_validation_is_not_push_validation(self):
        self.assertIsNone(select_run([run(1, event='pull_request')], REPO, SHA))
    def test_wrong_source_or_repo_rejected(self):
        self.assertIsNone(select_run([run(1, head_sha='b'*40), run(2, head_repository={'full_name':'other/fork'})], REPO, SHA))
    def test_latest_failed_run_overrides_old_success(self):
        selected = select_run([run(1), run(2, conclusion='failure')], REPO, SHA)
        with self.assertRaisesRegex(ValueError,'latest push CI failed'):evaluate(selected)
    def test_latest_pending_run_is_not_old_success(self):
        self.assertEqual(evaluate(select_run([run(1), run(2, status='queued', conclusion=None)], REPO, SHA)), 'wait')
    def test_latest_attempt_required(self):
        selected = select_run([run(1), run(1, run_attempt=2, conclusion='cancelled')], REPO, SHA)
        with self.assertRaises(ValueError):evaluate(selected)
    def test_success(self):
        self.assertEqual(evaluate(select_run([run(1)], REPO, SHA)), 'pass')
    def test_absent_waits(self):
        self.assertEqual(evaluate(None),'wait')


if __name__ == '__main__': unittest.main()
