package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSourcesDeploymentMigrationCannotBootstrapAtlas(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("SELECT EXISTS ").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	require.ErrorContains(t, CheckSourcesDeploymentMigrationPlan(context.Background(), db, []string{"246_sources_channel_monitor_sort_order.sql"}), "cannot create an Atlas baseline")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourcesDeploymentMigrationApprovalIsExact(t *testing.T) {
	const existing = "CREATE TABLE before(id int);"
	sum := sha256.Sum256([]byte(existing))
	ledger := map[string]string{"001.sql": hex.EncodeToString(sum[:])}
	files := fstest.MapFS{
		"001.sql": {Data: []byte(existing)},
		"002.sql": {Data: []byte("ALTER TABLE before ADD COLUMN new_column int;")},
		"003.sql": {Data: []byte("CREATE INDEX additive ON before(new_column);")},
	}
	require.NoError(t, checkSourcesDeploymentMigrationPlan(files, ledger, []string{"002.sql", "003.sql"}))
	for _, approved := range [][]string{nil, {"002.sql"}, {"002.sql", "003.sql", "004.sql"}, {"001.sql", "002.sql", "003.sql"}, {"002.sql", "002.sql"}, {"../002.sql"}} {
		require.Error(t, checkSourcesDeploymentMigrationPlan(files, ledger, approved))
	}
	require.Error(t, checkSourcesDeploymentMigrationPlan(files, nil, []string{"001.sql", "002.sql", "003.sql"}))
	ledger["001.sql"] = strings.Repeat("0", 64)
	require.ErrorContains(t, checkSourcesDeploymentMigrationPlan(files, ledger, []string{"002.sql", "003.sql"}), "checksum mismatch")
}

func TestSourcesPendingMigrationsListsOnlyUnrecordedNonEmptyFiles(t *testing.T) {
	files := fstest.MapFS{
		"001.sql":  {Data: []byte("CREATE TABLE before(id int);")},
		"003.sql":  {Data: []byte("ALTER TABLE before ADD COLUMN later int;")},
		"002.sql":  {Data: []byte("ALTER TABLE before ADD COLUMN new_column int;")},
		"004.sql":  {Data: []byte("  \n")},
		"notes.md": {Data: []byte("not a migration")},
	}
	pending, err := pendingSourcesMigrations(files, map[string]string{"001.sql": "recorded"})
	require.NoError(t, err)
	require.Equal(t, []string{"002.sql", "003.sql"}, pending)

	none, err := pendingSourcesMigrations(files, map[string]string{"001.sql": "a", "002.sql": "b", "003.sql": "c"})
	require.NoError(t, err)
	require.Empty(t, none)
	require.NotNil(t, none, "an empty result prints nothing but is still a valid list")
}
