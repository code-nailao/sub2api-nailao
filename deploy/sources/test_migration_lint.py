import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import migration_lint as ml  # noqa: E402


class MigrationLintTest(unittest.TestCase):
    def test_expanding_changes_pass(self):
        sql = """
        ALTER TABLE accounts ADD COLUMN tier text;
        CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON accounts(tier);
        CREATE TABLE IF NOT EXISTS t (id bigint);
        DROP INDEX IF EXISTS old_idx;
        """
        self.assertEqual([], ml.violations("300_add.sql", sql))

    def test_destructive_changes_fail(self):
        for sql, why in [("ALTER TABLE a DROP COLUMN b;", "drops a column"),
                         ("DROP TABLE a;", "drops a table"),
                         ("ALTER TABLE a RENAME COLUMN b TO c;", "renames"),
                         ("ALTER TABLE a RENAME TO b;", "renames"),
                         ("ALTER TABLE a ALTER COLUMN b TYPE bigint;", "changes a column type"),
                         ("ALTER TABLE a ALTER COLUMN b SET NOT NULL;", "NOT NULL"),
                         ("TRUNCATE a;", "truncates")]:
            found = ml.violations("301_x.sql", sql)
            self.assertTrue(found and why in found[0], (sql, found))

    def test_comments_and_contract_files_are_ignored(self):
        self.assertEqual([], ml.violations("302_x.sql", "-- DROP TABLE a;\n/* DROP COLUMN b */ SELECT 1;"))
        self.assertEqual([], ml.violations("303_contract_drop_b.sql", "ALTER TABLE a DROP COLUMN b;"))

    def test_cli_reports_and_honours_allow_list(self):
        with tempfile.TemporaryDirectory() as tmp:
            bad = Path(tmp) / "304_bad.sql"
            bad.write_text("ALTER TABLE a DROP COLUMN b;")
            self.assertEqual(1, ml.main([str(bad)]))
            original = ml.ALLOW_FILE
            try:
                ml.ALLOW_FILE = Path(tmp) / "allow.txt"
                ml.ALLOW_FILE.write_text("# header\n304_bad.sql reviewed upstream sync\n")
                self.assertEqual(0, ml.main([str(bad)]))
            finally:
                ml.ALLOW_FILE = original


if __name__ == "__main__":
    unittest.main()
