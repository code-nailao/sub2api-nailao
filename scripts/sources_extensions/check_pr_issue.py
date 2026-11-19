#!/usr/bin/env python3
"""Checks that a pull request description says which issue it belongs to.

The description is read from the PR_BODY environment variable. It passes with

1. a closing keyword and an issue reference, as GitHub reads them (`Closes #12`,
   `Fixes: owner/repo#12`, `Resolves <issue URL>`), or `Refs #12` for a link
   that closes nothing; or
2. a line `No issue: <reason>` whose reason is not empty.

HTML comments are ignored, so the hints in the pull request template do not
count as an answer. Only the form is checked: whether the number is a real
issue, and whether the reason is an accepted one, is for the reviewer. The rule
is in the "Issue 与 PR" section of docs/sources/WORKSPACE.md.

Nothing here uses the network, and nothing from the description is printed
except the matched reference.
"""
import os
import re
import sys

# An unclosed comment hides the rest of the description on GitHub as well.
COMMENT = re.compile(r'<!--.*?(?:-->|\Z)', re.S)
KEYWORD = r'(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?)'
REFERENCE = r'(?:(?:[\w.-]+/[\w.-]+)?#\d+|https://github\.com/[\w.-]+/[\w.-]+/issues/\d+)'
LINKED = re.compile(rf'(?<![\w-]){KEYWORD}:?[ \t]+{REFERENCE}(?!\w)', re.I)
# Markdown emphasis around the label and a full-width colon are both accepted.
NO_ISSUE = re.compile(r'(?<![\w-])no issue[*_`]*[ \t]*[:：](.*)$', re.I | re.M)
PLACEHOLDER = re.compile(r'<[^<>]*>')

HINT = ('the pull request description names no issue. Add `Closes #N` '
        '(`Refs #N` to link without closing) or a line `No issue: <reason>` '
        'to the description, then re-run this job.')


def verdict(body):
    """Returns (passed, detail) for a pull request description."""
    text = COMMENT.sub('', body or '')
    linked = LINKED.search(text)
    if linked:
        return True, 'linked with "' + linked.group(0) + '"'
    for reason in NO_ISSUE.findall(text):
        # A placeholder left as it was, such as `<reason>`, is not a reason.
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
