//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGroupUsageRollupIncludesArchivedOnlyHistory(t *testing.T) {
	ctx := context.Background()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
	todayStart := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, false)
	tx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = tx.Rollback() }()

	_, err := tx.ExecContext(ctx, `
		INSERT INTO groups (id) VALUES (10), (20);
		INSERT INTO users (id) VALUES (1);
		INSERT INTO usage_cleanup_group_daily (bucket_date, group_id, actual_cost) VALUES
			(DATE '2026-01-05', 10, 100), (DATE '2026-01-02', 20, 40);
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at) VALUES
			(1, 1, 10, 3, TIMESTAMPTZ '2026-03-08 12:00:00+08'),
			(2, 1, 10, 5, TIMESTAMPTZ '2026-03-09 12:00:00+08');
		UPDATE usage_group_rollup_state SET
			closed_before = DATE '1970-01-01',
			retained_from = TIMESTAMPTZ '1970-01-01 00:00:00+00',
			timezone_name = 'Asia/Shanghai'
		WHERE id = 1;
	`)
	require.NoError(t, err)
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(tx).SyncGroupUsageRollups(ctx, todayStart))

	var retainedDate string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT (retained_from AT TIME ZONE 'Asia/Shanghai')::date::text FROM usage_group_rollup_state WHERE id = 1`).Scan(&retainedDate))
	require.Equal(t, "2026-01-02", retainedDate, "raw-only MIN would hide the earlier archive-only group")
	rows, err := newUsageLogRepositoryWithSQL(nil, tx).GetAllGroupUsageSummary(ctx, todayStart)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, int64(10), rows[0].GroupID)
	require.InDelta(t, 108, rows[0].TotalCost, 0.0000001)
	require.InDelta(t, 5, rows[0].TodayCost, 0.0000001)
	require.InDelta(t, 3, rows[0].YesterdayCost, 0.0000001)
	require.Equal(t, int64(20), rows[1].GroupID)
	require.InDelta(t, 40, rows[1].TotalCost, 0.0000001)
}

func TestGroupUsageSummaryPreservesArchivesWhenNativeRawRetentionAdvances(t *testing.T) {
	ctx := context.Background()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
	todayStart := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, false)
	tx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = tx.Rollback() }()

	_, err := tx.ExecContext(ctx, `
		INSERT INTO groups (id) VALUES (10), (20);
		INSERT INTO users (id) VALUES (1);
		INSERT INTO usage_cleanup_group_daily (bucket_date, group_id, actual_cost) VALUES
			(DATE '2026-01-05', 10, 100), (DATE '2026-01-02', 20, 40);
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at) VALUES
			(1, 1, 10, 999, TIMESTAMPTZ '2026-01-08 12:00:00+08'),
			(2, 1, 10, 3, TIMESTAMPTZ '2026-03-08 12:00:00+08'),
			(3, 1, 10, 5, TIMESTAMPTZ '2026-03-09 12:00:00+08');
		INSERT INTO usage_group_daily_rollups (bucket_date, group_id, actual_cost) VALUES
			(DATE '2026-01-05', 10, 100),
			(DATE '2026-01-08', 10, 999),
			(DATE '2026-03-08', 10, 3);
		UPDATE usage_group_rollup_state SET
			closed_before = DATE '2026-03-09',
			retained_from = TIMESTAMPTZ '2026-02-01 00:00:00+08',
			expired_before = TIMESTAMPTZ '2026-02-01 00:00:00+08',
			timezone_name = 'Asia/Shanghai'
		WHERE id = 1;
	`)
	require.NoError(t, err)
	check := func() {
		t.Helper()
		rows, err := newUsageLogRepositoryWithSQL(nil, tx).GetAllGroupUsageSummary(ctx, todayStart)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.InDelta(t, 108, rows[0].TotalCost, 0.0000001, "include archived100 once, exclude expired raw999")
		require.InDelta(t, 5, rows[0].TodayCost, 0.0000001)
		require.InDelta(t, 3, rows[0].YesterdayCost, 0.0000001)
		require.InDelta(t, 40, rows[1].TotalCost, 0.0000001, "archive-only group remains visible")
	}
	check()

	// A native full rebuild removes expired mixed daily buckets. Preserved
	// private summaries must remain visible both before and after it.
	_, err = tx.ExecContext(ctx, `UPDATE usage_group_rollup_state SET closed_before = DATE '1970-01-01' WHERE id = 1`)
	require.NoError(t, err)
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(tx).SyncGroupUsageRollups(ctx, todayStart))
	check()
	var archives int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_cleanup_group_daily`).Scan(&archives))
	require.Equal(t, 2, archives)
}
