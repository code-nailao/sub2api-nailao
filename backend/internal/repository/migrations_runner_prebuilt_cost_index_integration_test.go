//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A bounded deployment may finish the exact concurrent index before invoking
// migrate-only. Native migration bookkeeping must accept that valid index
// without rebuilding it or altering any other historical migration checksum.
func TestSourcesPrebuiltCostIndexUsesNativeMigrationBookkeeping(t *testing.T) {
	ctx := context.Background()
	var checksum, before, after string
	var oidBefore, oidAfter int64
	var valid bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename=$1", usageLogsAPIKeyCostIndexMigration).Scan(&checksum))
	const history = "SELECT COALESCE(jsonb_object_agg(filename,checksum),'{}'::jsonb)::text FROM schema_migrations WHERE filename<>$1"
	require.NoError(t, integrationDB.QueryRowContext(ctx, history, usageLogsAPIKeyCostIndexMigration).Scan(&before))
	const index = "SELECT c.oid::bigint,i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid WHERE c.oid=to_regclass($1)"
	require.NoError(t, integrationDB.QueryRowContext(ctx, index, usageLogsAPIKeyCostIndex).Scan(&oidBefore, &valid))
	require.True(t, valid)
	_, err := integrationDB.ExecContext(ctx, "DELETE FROM schema_migrations WHERE filename=$1", usageLogsAPIKeyCostIndexMigration)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "INSERT INTO schema_migrations(filename,checksum) VALUES($1,$2) ON CONFLICT DO NOTHING", usageLogsAPIKeyCostIndexMigration, checksum)
	})
	require.NoError(t, CheckSourcesDeploymentMigrationPlan(ctx, integrationDB, []string{usageLogsAPIKeyCostIndexMigration}))
	require.NoError(t, ApplyMigrations(ctx, integrationDB))
	require.NoError(t, integrationDB.QueryRowContext(ctx, index, usageLogsAPIKeyCostIndex).Scan(&oidAfter, &valid))
	require.True(t, valid)
	require.Equal(t, oidBefore, oidAfter, "a valid prebuilt index must not be rebuilt")
	var recorded string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename=$1", usageLogsAPIKeyCostIndexMigration).Scan(&recorded))
	require.Equal(t, checksum, recorded)
	require.NoError(t, integrationDB.QueryRowContext(ctx, history, usageLogsAPIKeyCostIndexMigration).Scan(&after))
	require.Equal(t, before, after)
}
