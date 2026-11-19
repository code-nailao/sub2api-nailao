#!/usr/bin/env python3
"""Checks that a pull request description says which issue it belongs to.

The description is read from the PR_BODY environment variable. It passes with

1. a closing keyword and an issue reference, as GitHub reads them (`Closes #12`,
   `Fixes: owner/repo#12`, `Resolves <issue URL>`), `Refs #12` for a mention
   that closes nothing, or the `Reverts owner/repo#12` that GitHub's Revert
   button writes; or
2. a line that starts with `No issue:` and goes on to give the reason. An
   `Issue:` label, a list marker or emphasis may come before it; the phrase in
   the middle of a sentence does not count.

HTML comments are ignored, so the hints in the pull request template do not
count as an answer. Code spans and fences are not parsed. Only the form is
checked: whether the number is a real issue, and whether the reason is an
accepted one, is for the reviewer. The rule is in the "Issue 与 PR" section of
docs/sources/WORKSPACE.md.

Nothing here uses the network, and nothing from the description is printed
except the matched reference.
"""
import os
import re
import sys

# As GitHub renders them: a comment that starts a line runs to its end marker
# or, without one, hides the rest of the description; a comment inside a line
# needs its end marker in the same paragraph, or it is ordinary text.
COMMENT = re.compile(r'^ {0,3}<!--.*?(?:-->|\Z)|<!--(?:(?!\n[ \t]*\r?\n).)*?-->', re.S | re.M)
KEYWORD = r'(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?|reverts?)'
NAME = r'[A-Za-z0-9_.-]+'
NUMBER = r'[1-9][0-9]*'
REFERENCE = rf'(?:(?:{NAME}/{NAME})?#{NUMBER}|https://github\.com/{NAME}/{NAME}/issues/{NUMBER})'
LINKED = re.compile(rf'(?<![\w-]){KEYWORD}:?[ \t]+{REFERENCE}(?!\w)', re.I)
# A full-width colon is accepted after either label. No two neighbouring
# repeats can match the same character, which keeps a long run of `*` linear.
NO_ISSUE = re.compile(
    r'^[ \t>*_#+-]*(?:issue[*_]*[ \t]*[:：][ \t*_]*)?no issue[*_]*[ \t]*[:：](.*)$',
    re.I | re.M)
PLACEHOLDER = re.compile(r'<[^<>]*>')

HINT = ('the pull request description names no issue. Add `Closes #N` '
        '(`Refs #N` to mention one without closing it), or a line that starts '
        'with `No issue:` followed by the reason in plain words, then re-run '
        'this job.')


def verdict(body):
    """Returns (passed, detail) for a pull request description."""
    text = COMMENT.sub('', body or '')
    linked = LINKED.search(text)
    if linked:
        return True, 'linked with "' + linked.group(0) + '"'
    for reason in NO_ISSUE.findall(text):
        # GitHub hides text in angle brackets, so a reason has to stand outside
        # them; a placeholder left as it was, such as `<reason>`, is not one.
        if re.search(r'\w', PLACEHOLDER.sub('', reason)):
            return True, 'no issue, and a reason is given'
    return False, HINT


def main():
    passed, detail = verdict(os.environ.get('PR_BODY', ''))
    if passed:
        print('PR_ISSUE_PASS: ' + detail)
        return 0
    if os.environ.get('GITHUB_ACTIONS') == 'true':
        print('::error title=Pull request names no issue::' + detail)
    print('PR_ISSUE_FAIL: ' + detail)
    return 1


if __name__ == '__main__':
    sys.exit(main())
