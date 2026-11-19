package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestSourcesServingMigrationVerificationNeverAppliesMissingDDL(t *testing.T) {
	sqlText := "CREATE TABLE must_not_be_created(id int);"
	digest := sha256.Sum256([]byte(sqlText))
	for _, tc := range []struct {
		name, checksum string
		present, valid bool
	}{
		{"already_applied", hex.EncodeToString(digest[:]), true, true},
		{"pending", "", false, false},
		{"changed", strings.Repeat("0", 64), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"filename", "checksum"})
			if tc.present {
				rows.AddRow("001.sql", tc.checksum)
			}
			mock.ExpectQuery("SELECT filename, checksum FROM schema_migrations").WillReturnRows(rows)
			mock.ExpectCommit() // The read-only ledger snapshot precedes local checksum validation.
			err = verifySourcesMigrations(context.Background(), db, fstest.MapFS{"001.sql": {Data: []byte(sqlText)}})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet(), "serving must perform no migration, ledger write, or bootstrap DDL")
		})
	}
}

func TestSourcesServingPreservesNativeHistoricalChecksumRules(t *testing.T) {
	const name = "054_drop_legacy_cache_columns.sql"
	content, err := migrations.FS.ReadFile(name)
	require.NoError(t, err)
	files := fstest.MapFS{name: {Data: content}}
	require.NoError(t, verifySourcesMigrationLedger(files, map[string]string{name: "182c193f3359946cf094090cd9e57d5c3fd9abaffbc1e8fc378646b8a6fa12b4"}))
	require.Error(t, verifySourcesMigrationLedger(files, map[string]string{name: strings.Repeat("f", 64)}))
	files[name].Data = append(content, []byte("\nSELECT 123456;\n")...)
	require.Error(t, verifySourcesMigrationLedger(files, map[string]string{name: "182c193f3359946cf094090cd9e57d5c3fd9abaffbc1e8fc378646b8a6fa12b4"}))
}

func TestSourcesServingReadsOwnerSecretWithoutBootstrapping(t *testing.T) {
	client := newSecuritySecretTestClient(t)
	cfg := &config.Config{}
	cfg.JWT.Secret = strings.Repeat("candidate", 5)
	require.Error(t, readSourcesBootstrapSecrets(context.Background(), client, cfg))
	count, err := client.SecuritySecret.Query().Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, count, "serving must not persist its own signing key")
	owner := &config.Config{}
	require.NoError(t, ensureBootstrapSecrets(context.Background(), client, owner))
	require.NoError(t, readSourcesBootstrapSecrets(context.Background(), client, cfg))
	require.Equal(t, owner.JWT.Secret, cfg.JWT.Secret)
}
