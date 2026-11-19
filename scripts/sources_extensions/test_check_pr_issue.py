import os
from pathlib import Path
import subprocess
import sys
import unittest

from check_pr_issue import verdict

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = Path(__file__).with_name('check_pr_issue.py')
TEMPLATE = ROOT / '.github/pull_request_template.md'


def passes(body):
    return verdict(body)[0]


class LinkedIssueTests(unittest.TestCase):
    def test_closing_keywords(self):
        for keyword in ('Close', 'Closes', 'Closed', 'Fix', 'Fixes', 'Fixed',
                        'Resolve', 'Resolves', 'Resolved', 'closes', 'FIXES'):
            with self.subTest(keyword=keyword):
                self.assertTrue(passes(f'{keyword} #12'))

    def test_refs_links_without_closing(self):
        self.assertTrue(passes('Refs #12'))
        self.assertTrue(passes('Ref #12'))

    def test_reference_forms(self):
        for body in ('Closes: #12', 'Fixes example-owner/example-repo#12',
                     'Resolves https://github.com/example-owner/example-repo/issues/12',
                     'Issue: Closes #12', '**Issue:** Closes #12', '- Closes #12, closes #15',
                     'First paragraph.\r\n\r\nIssue: Fixes #7\r\n\r\n## Changes\r\n'):
            with self.subTest(body=body):
                self.assertTrue(passes(body))

    def test_reports_the_matched_reference_only(self):
        self.assertEqual(verdict('Summary line\nIssue: Closes #12\nMore text'),
                         (True, 'linked with "Closes #12"'))

    def test_a_number_without_a_keyword_is_not_a_link(self):
        for body in ('#12', 'See #12 and #15.', 'Merged main (with #4 and #7).',
                     'Issue: #12', 'Related to #12'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_a_keyword_without_a_number_is_not_a_link(self):
        for body in ('Closes', 'Closes #', 'Closes #N', 'Fixes the bug', 'Closes 12',
                     'Closes\n#12', 'Closes #12abc'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_a_keyword_inside_another_word_is_not_a_link(self):
        for body in ('hotfix #12', 'prefix #12', 'bug-fix #12', 'prefs #12', 'unresolved #12'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))


class NoIssueTests(unittest.TestCase):
    def test_reason_given(self):
        for body in ('No issue: typo in the developer guide', 'no issue: dependency bump',
                     'Issue: No issue: release preparation', '**No issue:** typo',
                     '**No issue**: typo', 'No issue：修正文档里的错别字',
                     '## Summary\r\n\r\nNo issue: CI maintenance\r\n\r\n## Checks\r\n',
                     'No issue: rename the <Panel> prop'):
            with self.subTest(body=body):
                self.assertTrue(passes(body))

    def test_reason_missing(self):
        for body in ('No issue', 'No issue:', 'No issue:   ', 'No issue: \r\nNext line',
                     'No issue:\nreason on the next line', '**No issue:**', 'No issue: -',
                     'No issue: <reason>', 'No issue: <原因>', 'Issue: No issue: `<reason>`'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_other_wording_is_not_the_marker(self):
        for body in ('No issues: none', 'There is no-issue: here', 'casino issue: chips'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))


class DescriptionTests(unittest.TestCase):
    def test_empty_description_fails_with_an_instruction(self):
        for body in (None, '', '   \n\n'):
            with self.subTest(body=body):
                passed, detail = verdict(body)
                self.assertFalse(passed)
                self.assertIn('Closes #N', detail)
                self.assertIn('No issue: <reason>', detail)

    def test_html_comments_do_not_count(self):
        self.assertFalse(passes('Issue: <!-- Closes #12 or No issue: typo -->'))
        self.assertFalse(passes('<!--\nCloses #12\nNo issue: typo\n-->\nText'))
        self.assertFalse(passes('Text\n<!-- unclosed\nCloses #12'))
        self.assertTrue(passes('Issue: <!-- hint --> Closes #12'))
        self.assertTrue(passes('Issue: <!-- hint -->No issue: typo'))

    def test_untouched_template_fails(self):
        self.assertFalse(passes(TEMPLATE.read_text(encoding='utf-8')))

    def test_filled_template_passes(self):
        template = TEMPLATE.read_text(encoding='utf-8')
        self.assertEqual(template.count('\nIssue: <!--'), 1)
        self.assertTrue(passes(template.replace('\nIssue: <!--', '\nIssue: Closes #12 <!--')))
        self.assertTrue(passes(template.replace('\nIssue: <!--', '\nIssue: No issue: typo <!--')))


class CommandTests(unittest.TestCase):
    def run_with(self, body, **extra):
        env = {key: value for key, value in os.environ.items()
               if key not in ('PR_BODY', 'GITHUB_ACTIONS')}
        if body is not None:
            env['PR_BODY'] = body
        env.update(extra)
        return subprocess.run([sys.executable, str(SCRIPT)], env=env, capture_output=True, text=True)

    def test_pass(self):
        result = self.run_with('Closes #12')
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, 'PR_ISSUE_PASS: linked with "Closes #12"\n')

    def test_fail_without_the_variable(self):
        result = self.run_with(None)
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stdout.startswith('PR_ISSUE_FAIL: '))
        self.assertNotIn('::error', result.stdout)

    def test_fail_annotates_the_run_on_github(self):
        result = self.run_with('No issue:', GITHUB_ACTIONS='true')
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stdout.startswith('::error title='))

    def test_description_is_never_echoed(self):
        # A line the runner would read as a workflow command must not reach the log.
        for body in ('::error::spoofed\n', '::add-mask::spoofed\nNo issue: spoofed reason\n'):
            with self.subTest(body=body):
                self.assertNotIn('spoofed', self.run_with(body, GITHUB_ACTIONS='true').stdout)


if __name__ == '__main__':
    unittest.main()
