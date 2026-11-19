package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

// A serving candidate can warm beside the owner but cannot bootstrap or migrate
// their shared database. Verify the native ledger in a read-only transaction,
// preserving the exact native historical checksum compatibility rules.
func verifySourcesMigrations(ctx context.Context, db *sql.DB, files fs.FS) error {
	applied, err := readSourcesMigrationLedger(ctx, db)
	if err != nil {
		return err
	}
	return verifySourcesMigrationLedger(files, applied)
}

func readSourcesMigrationLedger(ctx context.Context, db *sql.DB) (map[string]string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT filename, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("deployment requires an initialized migration ledger: %w", err)
	}
	applied := map[string]string{}
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			_ = rows.Close()
			return nil, err
		}
		applied[name] = checksum
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return applied, nil
}

func verifySourcesMigrationLedger(files fs.FS, applied map[string]string) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	for _, name := range names {
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		trimmed := strings.TrimSpace(string(content))
		if trimmed == "" {
			continue
		}
		checksum := sha256.Sum256([]byte(trimmed))
		expected := hex.EncodeToString(checksum[:])
		actual, exists := applied[name]
		if !exists {
			return fmt.Errorf("serving cannot apply pending migration %s; prepare the database before candidate startup", name)
		}
		if actual != expected && !isMigrationChecksumCompatible(name, actual, expected) {
			return fmt.Errorf("serving migration %s checksum mismatch", name)
		}
	}
	return nil
}

func readSourcesBootstrapSecrets(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	stored, err := querySecuritySecretWithRetry(ctx, client, securitySecretKeyJWT)
	if err != nil {
		return fmt.Errorf("serving requires the owner's persisted JWT secret: %w", err)
	}
	value := strings.TrimSpace(stored.Value)
	if len(value) < 32 {
		return fmt.Errorf("stored JWT secret must be at least 32 bytes")
	}
	cfg.JWT.Secret = value
	return nil
}
