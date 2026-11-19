package repository

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestApplyMigrationsFS_RejectionReleaseDueIndexIsNonTransactionalAndDropsInvalidIndex(t *testing.T) {
	content, err := fs.ReadFile(migrations.FS, upstreamRejectionReleaseDueIndexMigration)
	require.NoError(t, err)
	nonTx, err := validateMigrationExecutionMode(upstreamRejectionReleaseDueIndexMigration, string(content))
	require.NoError(t, err)
	require.True(t, nonTx, "the index on a hot table must be built without blocking writers")
	columns, err := fs.ReadFile(migrations.FS, "272_upstream_rejection_release_deadline.sql")
	require.NoError(t, err)
	nonTx, err = validateMigrationExecutionMode("272_upstream_rejection_release_deadline.sql", string(columns))
	require.NoError(t, err)
	require.False(t, nonTx)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	prepareMigrationsBootstrapExpectations(mock)
	mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").
		WithArgs(upstreamRejectionReleaseDueIndexMigration).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT EXISTS \\(").
		WithArgs(upstreamRejectionReleaseDueIndex).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("DROP INDEX CONCURRENTLY IF EXISTS idx_upstream_rejection_release_due").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_upstream_rejection_release_due").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO schema_migrations \\(filename, checksum\\) VALUES \\(\\$1, \\$2\\)").
		WithArgs(upstreamRejectionReleaseDueIndexMigration, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").
		WithArgs(migrationsAdvisoryLockID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err = applyMigrationsFS(context.Background(), db, fstest.MapFS{upstreamRejectionReleaseDueIndexMigration: &fstest.MapFile{Data: content}})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
