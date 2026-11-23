import os
from pathlib import Path
import subprocess
import sys
import time
import unittest
from unittest import mock

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

    def test_refs_mentions_without_closing(self):
        self.assertTrue(passes('Refs #12'))
        self.assertTrue(passes('Ref #12'))

    def test_the_revert_button_description_passes_as_written(self):
        self.assertTrue(passes('Reverts example-owner/example-repo#12'))
        self.assertTrue(passes('Reverts #12'))
        self.assertFalse(passes('Revert "fix: stream timing"'))

    def test_reference_forms(self):
        for body in ('Closes: #12', 'Fixes example-owner/example-repo#12',
                     'Resolves https://github.com/example-owner/example-repo/issues/12',
                     'Issue: Closes #12', '**Issue:** Closes #12', '- Closes #12',
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
                     'Closes\n#12', 'Closes #12abc', 'Closes #0', 'Closes #012',
                     'Closes #１２', 'Closes #12３'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_a_keyword_inside_another_word_is_not_a_link(self):
        for body in ('hotfix #12', 'prefix #12', 'bug-fix #12', 'prefs #12', 'unresolved #12'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))


class OneClosingIssueTests(unittest.TestCase):
    """A pull request closes exactly one issue; a second issue means a split."""

    def test_no_closing_keyword_with_a_reason_passes(self):
        self.assertTrue(passes('No issue: typo in the developer guide'))

    def test_exactly_one_closing_keyword_passes(self):
        self.assertTrue(passes('Closes #12'))

    def test_the_same_issue_twice_counts_once(self):
        for body in ('Closes #12, closes #12', 'Fixes #12\nCloses #12',
                     'Closes example-owner/example-repo#12 and closes example-owner/example-repo#12',
                     'Closes https://github.com/example-owner/example-repo/issues/12, '
                     'closes example-owner/example-repo#12'):
            with self.subTest(body=body):
                self.assertTrue(passes(body))

    def test_this_repository_is_named_by_the_environment(self):
        # The workflow passes REPO, so a bare number is that repository's issue.
        with mock.patch.dict(os.environ, {'REPO': 'example-owner/example-repo'}):
            self.assertTrue(passes('Closes #12, closes example-owner/example-repo#12'))

    def test_the_same_number_in_another_repository_is_another_issue(self):
        with mock.patch.dict(os.environ, {'REPO': 'example-owner/example-repo'}):
            for body in ('Closes #12, closes other-owner/other-repo#12',
                         'Closes example-owner/example-repo#12, closes example-owner/other-repo#12',
                         'Closes https://github.com/other-owner/other-repo/issues/12, closes #12'):
                with self.subTest(body=body):
                    passed, detail = verdict(body)
                    self.assertFalse(passed)
                    self.assertIn('2 issues', detail)

    def test_a_number_too_long_for_an_integer_is_still_one_issue(self):
        # Nothing issues it, but the check reads numbers as text and must not
        # crash on one long enough for int() to refuse.
        number = '1' * 5000
        self.assertTrue(passes(f'Closes #{number}'))
        self.assertFalse(passes(f'Closes #{number}, closes #12'))

    def test_two_different_issues_fail_with_what_to_change(self):
        passed, detail = verdict('Closes #12, closes #15')
        self.assertFalse(passed)
        # The message names every issue and says which one to keep.
        self.assertIn('#12', detail)
        self.assertIn('#15', detail)
        self.assertIn('Closes #12', detail)
        self.assertIn('separate pull request', detail)

    def test_the_first_closing_reference_is_the_one_to_keep(self):
        passed, detail = verdict('Fixes #7 and closes #9')
        self.assertFalse(passed)
        self.assertIn('Fixes #7', detail)
        self.assertIn('separate pull request for #9', detail)

    def test_refs_does_not_count_as_closing(self):
        for body in ('Refs #12\nCloses #15', 'Closes #15\nRefs #12'):
            with self.subTest(body=body):
                self.assertTrue(passes(body))

    def test_only_the_reference_after_a_keyword_closes(self):
        # GitHub closes only #12 here, so the check counts one.
        self.assertTrue(passes('Closes #12, #15'))


class CodeTests(unittest.TestCase):
    """GitHub shows code without links, so an example in a description is not
    an answer either."""

    def test_a_keyword_inside_a_span_or_a_fence_does_not_count(self):
        for body in ('Issue: `Closes #12`',
                     'Issue: ```Closes #12```',
                     'Example:\n\n```\nCloses #12\n```\n',
                     'Example:\r\n\r\n~~~\r\nCloses #12\r\n~~~\r\n',
                     'Example:\n\n```md\nIssue: Closes #12\n```\n',
                     'Example:\n\n~~~\n```\nCloses #12\n```\n~~~\n',
                     'Example:\n\n- `Closes #12`\n',
                     'Example:\n\n```\nNo issue: typo\n```\n'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_a_reference_outside_code_still_counts(self):
        self.assertTrue(passes('`Closes #12` is how it is written.\n\nCloses #15'))
        self.assertTrue(passes('```\nCloses #12\n```\n\nCloses #15'))

    def test_an_unclosed_fence_runs_to_the_end(self):
        self.assertFalse(passes('Example:\n\n```\nCloses #12\n'))
        self.assertTrue(passes('Closes #12\n\n```\nunclosed example'))

    def test_an_unmatched_backtick_is_literal(self):
        self.assertTrue(passes('A lone ` sign.\n\nCloses #12'))


class NoIssueTests(unittest.TestCase):
    def test_reason_given(self):
        for body in ('No issue: typo in the developer guide', 'no issue: dependency bump',
                     'Issue: No issue: release preparation', '**No issue:** typo',
                     '**No issue**: typo', 'No issue：修正文档里的错别字',
                     '**Issue:** No issue: typo', 'Issue: **No issue:** typo',
                     'Issue：No issue：发版准备', '- No issue: typo', '> No issue: typo',
                     '  No issue: typo', 'Summary first.\nIssue: No issue: typo\nMore.',
                     '## Summary\r\n\r\nNo issue: CI maintenance\r\n\r\n## Checks\r\n',
                     'No issue: rename the <Panel> prop'):
            with self.subTest(body=body):
                self.assertTrue(passes(body))

    def test_reason_missing(self):
        for body in ('No issue', 'No issue:', 'No issue:   ', 'No issue: \r\nNext line',
                     'No issue:\nreason on the next line', '**No issue:**', 'No issue: -',
                     'No issue: <reason>', 'No issue: <原因>', 'Issue: No issue: `<reason>`',
                     'No issue: <typo in the guide>'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_other_wording_is_not_the_marker(self):
        for body in ('No issues: none', 'There is no-issue: here', 'casino issue: chips'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))

    def test_the_phrase_inside_a_sentence_is_not_the_marker(self):
        for body in ('There is no issue: all tests pass.',
                     'A description needs a keyword or a line `No issue:` with a reason.',
                     '- `No issue:` with a reason also passes',
                     '正文要么关联 issue，要么写一行 `No issue:` 加原因。',
                     'Tracking issue: no issue: none yet'):
            with self.subTest(body=body):
                self.assertFalse(passes(body))


class DescriptionTests(unittest.TestCase):
    def test_empty_description_fails_with_an_instruction(self):
        for body in (None, '', '   \n\n'):
            with self.subTest(body=body):
                passed, detail = verdict(body)
                self.assertFalse(passed)
                self.assertIn('Closes #N', detail)
                self.assertIn('`No issue:`', detail)
                # Text in angle brackets is hidden on GitHub and does not count
                # as a reason, so the instruction must not suggest writing it.
                self.assertNotIn('<', detail)

    def test_html_comments_do_not_count(self):
        self.assertFalse(passes('Issue: <!-- Closes #12 or No issue: typo -->'))
        self.assertFalse(passes('<!--\nCloses #12\nNo issue: typo\n-->\nText'))
        self.assertTrue(passes('Issue: <!-- hint --> Closes #12'))
        self.assertTrue(passes('Issue: <!-- hint -->No issue: typo'))
        self.assertTrue(passes('<!-- one -->\r\n\r\nIssue: Closes #12 <!-- two -->\r\n'))

    def test_an_unclosed_comment_hides_the_rest_only_from_a_line_start(self):
        self.assertFalse(passes('Text\n<!-- unclosed\nCloses #12'))
        self.assertFalse(passes('Text\n  <!-- unclosed\n\nNo issue: typo'))
        # Inside a line it is ordinary text, as GitHub renders it.
        self.assertTrue(passes('Issue: <!-- hint Closes #12'))
        self.assertTrue(passes('Strip `<!--` from the output.\n\nCloses #12'))
        self.assertTrue(passes('Strip `<!--` from the output.\r\n\r\nCloses #12\r\n\r\n<!-- note -->'))

    def test_long_runs_stay_fast(self):
        for body in ('*' * 60000 + 'no issue', 'Issue:' + '*' * 30000 + ' ' * 30000 + 'no issue',
                     'closes' + ' ' * 60000 + '#', 'No issue:\n' * 5000):
            with self.subTest(body=body[:12]):
                started = time.monotonic()
                self.assertFalse(passes(body))
                self.assertLess(time.monotonic() - started, 2)

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

    def test_several_issues_fail_with_instructions(self):
        result = self.run_with('Closes #12, closes #15')
        self.assertEqual(result.returncode, 1)
        self.assertIn('#12', result.stdout)
        self.assertIn('#15', result.stdout)
        self.assertIn('separate pull request for #15', result.stdout)

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
