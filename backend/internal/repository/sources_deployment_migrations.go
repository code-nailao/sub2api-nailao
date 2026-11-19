package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/migrations"
)

// CheckSourcesDeploymentMigrationPlan requires an initialized ledger and an
// exact reviewed pending set. It never applies SQL or relaxes native historical
// checksum compatibility. The caller must serialize deployment preparation;
// ApplyMigrations also obtains the original PostgreSQL migration advisory lock.
func CheckSourcesDeploymentMigrationPlan(ctx context.Context, db *sql.DB, approved []string) error {
	var initialized bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM atlas_schema_revisions)").Scan(&initialized); err != nil {
		return fmt.Errorf("deployment requires an initialized Atlas ledger: %w", err)
	}
	if !initialized {
		return fmt.Errorf("deployment cannot create an Atlas baseline")
	}
	applied, err := readSourcesMigrationLedger(ctx, db)
	if err != nil {
		return err
	}
	return checkSourcesDeploymentMigrationPlan(migrations.FS, applied, approved)
}

func checkSourcesDeploymentMigrationPlan(files fs.FS, applied map[string]string, approved []string) error {
	if len(applied) == 0 || len(approved) == 0 {
		return fmt.Errorf("initialized ledger and explicitly approved pending migrations are required")
	}
	allowed := make(map[string]bool, len(approved))
	for _, name := range approved {
		if !fs.ValidPath(name) || strings.Contains(name, "/") || !strings.HasSuffix(name, ".sql") || allowed[name] {
			return fmt.Errorf("invalid or duplicate approved migration: %q", name)
		}
		allowed[name] = true
	}
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	proposed := make(map[string]string, len(applied)+len(approved))
	for name, checksum := range applied {
		proposed[name] = checksum
	}
	var pending []string
	for _, name := range names {
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(content)) == "" {
			continue
		}
		if _, exists := applied[name]; !exists {
			pending = append(pending, name)
			if !allowed[name] {
				return fmt.Errorf("unapproved pending migration: %s", name)
			}
			checksum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
			proposed[name] = hex.EncodeToString(checksum[:])
		}
	}
	// Reuse the unchanged native compatibility policy before any migration writes.
	if err := verifySourcesMigrationLedger(files, proposed); err != nil {
		return err
	}
	if len(pending) != len(allowed) {
		sort.Strings(pending)
		return fmt.Errorf("pending migration set differs from approval: %v", pending)
	}
	return nil
}

// ListSourcesPendingMigrations returns the embedded, non-empty migration
// filenames that the ledger has not recorded yet, sorted. It only reads; the
// result feeds an explicit -migrate-only approval, which repeats every check
// (initialized ledger, exact set, native checksum compatibility) before SQL.
func ListSourcesPendingMigrations(ctx context.Context, db *sql.DB) ([]string, error) {
	applied, err := readSourcesMigrationLedger(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(applied) == 0 {
		return nil, fmt.Errorf("deployment requires an initialized migration ledger")
	}
	return pendingSourcesMigrations(migrations.FS, applied)
}

func pendingSourcesMigrations(files fs.FS, applied map[string]string) ([]string, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	pending := []string{}
	for _, name := range names {
		if _, exists := applied[name]; exists {
			continue
		}
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(content)) == "" {
			continue
		}
		pending = append(pending, name)
	}
	sort.Strings(pending)
	return pending, nil
}
