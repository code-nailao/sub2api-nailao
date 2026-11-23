#!/usr/bin/env python3
"""Checks that a pull request description says which issue it belongs to.

The description is read from the PR_BODY environment variable. It passes with

1. exactly one issue closed by a closing keyword, as GitHub reads them
   (`Closes #12`, `Fixes: owner/repo#12`, `Resolves <issue URL>`). The same
   issue named more than once counts once, and an issue is a repository and a
   number: `#12` and `owner/repo#12` are the same only when the owner and
   repository match. A reference without them is this repository's, named by
   the `REPO` environment variable the workflow passes. Two different issues
   fail, since a pull request has one concern and is split rather than closing
   both. A `Refs #12` that closes nothing, or the `Reverts owner/repo#12` that
   GitHub's Revert button writes, passes when no closing keyword is present; or
2. a line that starts with `No issue:` and goes on to give the reason. An
   `Issue:` label, a list marker or emphasis may come before it; the phrase in
   the middle of a sentence does not count.

HTML comments, fenced code blocks and code spans are ignored: GitHub shows none
of them as links, so the hints in the pull request template and the examples in
a description do not answer for it. Only the form is checked: whether the
number is a real issue, and whether the reason is an accepted one, is for the
reviewer. The rule is in the "Issue 与 PR" section of
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
# GitHub closes an issue for the first three; the other two only reference.
CLOSING_KEYWORDS = frozenset(('close', 'closes', 'closed', 'fix', 'fixes', 'fixed',
                              'resolve', 'resolves', 'resolved'))
NAME = r'[A-Za-z0-9_.-]+'
NUMBER = r'[1-9][0-9]*'
REFERENCE = rf'(?:(?:{NAME}/{NAME})?#{NUMBER}|https://github\.com/{NAME}/{NAME}/issues/{NUMBER})'
LINKED = re.compile(rf'(?<![\w-])({KEYWORD}):?[ \t]+({REFERENCE})(?!\w)', re.I)
# The reference is read back as a repository and a number. The number stays a
# string: it is canonical without leading zeros, and a number long enough to
# overflow int() is still just a number no issue has.
SHORT_REFERENCE = re.compile(rf'^(?:(?P<slug>{NAME}/{NAME}))?#(?P<number>{NUMBER})$')
URL_REFERENCE = re.compile(
    rf'^https://github\.com/(?P<slug>{NAME}/{NAME})/issues/(?P<number>{NUMBER})$')
FENCE = re.compile(r'^ {0,3}(`{3,}|~{3,})')
BACKTICKS = re.compile(r'`+')
# A full-width colon is accepted after either label. No two neighbouring
# repeats can match the same character, which keeps a long run of `*` linear.
NO_ISSUE = re.compile(
    r'^[ \t>*_#+-]*(?:issue[*_]*[ \t]*[:：][ \t*_]*)?no issue[*_]*[ \t]*[:：](.*)$',
    re.I | re.M)
PLACEHOLDER = re.compile(r'<[^<>]*>')

HINT = ('the pull request description names no issue. Add `Closes #N` for the '
        'one issue this pull request settles (`Refs #N` to mention one without '
        'closing it), or a line that starts with `No issue:` followed by the '
        'reason in plain words, then re-run this job.')


def blank_spans(text):
    """Replaces code spans, inside a line or across lines, with a space.

    A span opens with a run of backticks and closes with an equally long run, as
    Markdown reads it. An unclosed run is literal, and pairs that nest blank
    their whole outer span.
    """
    opened = {}
    spans = []
    for match in BACKTICKS.finditer(text):
        length = match.end() - match.start()
        start = opened.pop(length, None)
        if start is None:
            opened[length] = match.start()
        else:
            spans.append((start, match.end()))
    if not spans:
        return text
    out = []
    position = 0
    for start, end in sorted(spans):
        if start < position:
            position = max(position, end)
            continue
        out.append(text[position:start])
        out.append(' ')
        position = end
    out.append(text[position:])
    return ''.join(out)


def strip_code(text):
    """Drops fenced blocks and code spans, which GitHub never links from."""
    kept = []
    fence = None
    for line in text.splitlines():
        opening = FENCE.match(line)
        if fence is None:
            if opening:
                fence = opening.group(1)
            else:
                kept.append(line)
        elif (opening and opening.group(1)[0] == fence[0]
              and len(opening.group(1)) >= len(fence)
              and not line[opening.end():].strip()):
            fence = None
        # Every other line of an open fence is code, and an unclosed fence runs
        # to the end of the description.
    return blank_spans('\n'.join(kept))


def issue_in(reference):
    """Returns ((repository, number), display) for a matched reference."""
    match = SHORT_REFERENCE.match(reference) or URL_REFERENCE.match(reference)
    number = match.group('number')
    slug = match.group('slug')
    repository = (slug or os.environ.get('REPO', '')).lower()
    # The message repeats the reference as it was written.
    return (repository, number), (f'{slug}#{number}' if slug else f'#{number}')


def verdict(body):
    """Returns (passed, detail) for a pull request description."""
    text = strip_code(COMMENT.sub('', body or ''))
    closing = {}  # (repository, number) -> the first reference that closes it
    mention = None
    for match in LINKED.finditer(text):
        issue, display = issue_in(match.group(2))
        if match.group(1).lower() in CLOSING_KEYWORDS:
            closing.setdefault(issue, (match.group(0), display))
        elif mention is None:
            mention = match.group(0)
    if len(closing) == 1:
        return True, 'linked with "' + next(iter(closing.values()))[0] + '"'
    if len(closing) > 1:
        items = list(closing.values())
        keep, keep_display = items[0]
        others = [display for _, display in items[1:]]
        listed = ', '.join([keep_display] + others)
        if len(others) == 1:
            move = f'open a separate pull request for {others[0]}'
        else:
            move = ('open separate pull requests for '
                    + ', '.join(others[:-1]) + f' and {others[-1]}')
        return False, (f'the pull request description closes {len(items)} issues '
                       f'({listed}). One pull request closes one issue: keep only '
                       f'"{keep}" here, {move}, and re-run this job.')
    if mention:
        return True, 'linked with "' + mention + '"'
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
        print('::error title=Pull request must name exactly one issue::' + detail)
    print('PR_ISSUE_FAIL: ' + detail)
    return 1


if __name__ == '__main__':
    sys.exit(main())
