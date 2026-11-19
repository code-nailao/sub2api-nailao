#!/usr/bin/env python3
"""Reject new migrations that would break the release still running.

deploy.py applies pending migrations while the previous release keeps
serving, so a migration must only expand the schema. Statements that remove
or narrow what the old code reads or writes belong in a later, explicitly
marked "contract" migration (filename contains "contract"), which deploy.py
runs only with --allow-contract. Reviewed exceptions, such as destructive
statements inside an upstream sync, go in migration-allow.txt with a reason.

    migration_lint.py --base origin/main     # files added since base
    migration_lint.py path/to/001_x.sql ...  # explicit files
"""
from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
MIGRATIONS = "backend/migrations"
ALLOW_FILE = Path(__file__).resolve().parent / "migration-allow.txt"
RULES = [
    (re.compile(r"\bDROP\s+TABLE\b", re.I), "drops a table"),
    (re.compile(r"\bDROP\s+COLUMN\b", re.I), "drops a column"),
    (re.compile(r"\bRENAME\s+(COLUMN\b|TO\b)", re.I), "renames a table or column"),
    (re.compile(r"\bALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b", re.I), "changes a column type"),
    (re.compile(r"\bSET\s+NOT\s+NULL\b", re.I), "adds NOT NULL to an existing column"),
    (re.compile(r"\bTRUNCATE\b", re.I), "truncates a table"),
]


def strip_comments(sql: str) -> str:
    sql = re.sub(r"/\*.*?\*/", " ", sql, flags=re.S)
    return re.sub(r"--[^\n]*", " ", sql)


def violations(name: str, sql: str) -> list[str]:
    if "contract" in Path(name).name.lower():
        return []
    body = strip_comments(sql)
    return [f"{name}: {why}" for pattern, why in RULES if pattern.search(body)]


def allowed() -> set[str]:
    if not ALLOW_FILE.exists():
        return set()
    return {line.split()[0] for line in ALLOW_FILE.read_text().splitlines() if line.strip() and not line.startswith("#")}


def added_since(base: str) -> list[Path]:
    out = subprocess.check_output(["git", "-C", str(ROOT), "diff", "--name-only", "--diff-filter=A",
                                   f"{base}...HEAD", "--", MIGRATIONS], text=True)
    return [ROOT / line for line in out.splitlines() if line.endswith(".sql")]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", help="git ref; check migrations added since it")
    parser.add_argument("files", nargs="*")
    args = parser.parse_args(argv)
    files = [Path(f) for f in args.files] + (added_since(args.base) if args.base else [])
    skip = allowed()
    problems = []
    for path in files:
        if path.name in skip:
            continue
        problems += violations(path.name, path.read_text())
    if problems:
        print("Migrations must only expand the schema while the previous release runs:", file=sys.stderr)
        for problem in problems:
            print("  - " + problem, file=sys.stderr)
        print("Move the change to a later migration whose name contains 'contract', or add a reviewed "
              "entry to deploy/sources/migration-allow.txt.", file=sys.stderr)
        return 1
    print(f"MIGRATION_LINT_PASS: {len(files)} new migration(s) checked")
    return 0


if __name__ == "__main__":
    sys.exit(main())
