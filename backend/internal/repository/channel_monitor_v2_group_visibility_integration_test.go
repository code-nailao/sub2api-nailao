//go:build integration

package repository

import (
	"context"
	"database/sql"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2GroupVisibilityPostgres(t *testing.T) {
	ctx := t.Context()
	db := channelMonitorV2VisibilityDB(t)
	repo := NewChannelMonitorV2Repository(db)
	svc := service.NewChannelMonitorV2Service(repo)
	_, err := db.ExecContext(ctx, `
		INSERT INTO channel_monitor_v2_watermarks (id, data_through, last_successful_at)
		VALUES (1, NOW(), NOW())`)
	require.NoError(t, err)

	const platform, model = "fixture-platform", "fixture-model"
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: platform, Enabled: true, Models: []string{model}}},
	}
	for _, window := range []string{"90m", "24h", "7d", "30d"} {
		t.Run(window, func(t *testing.T) {
			ctx := t.Context()
			filter, err := svc.ParseFilter(window, nil, nil, nil)
			require.NoError(t, err)
			bucketStart := filter.End.Add(-2 * filter.Bucket)
			createGroup := func(name string) int64 {
				var id int64
				err := db.QueryRowContext(ctx, `INSERT INTO groups (name, platform) VALUES ($1, $2) RETURNING id`, name, platform).Scan(&id)
				require.NoError(t, err)
				return id
			}
			activeName, deletedName := "active-"+window, "removed-"+window
			active, deleted, disabled := createGroup(activeName), createGroup(deletedName), createGroup("disabled-"+window)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), `TRUNCATE groups, channel_monitor_v2_metrics_rollup, channel_monitor_v2_latency_histograms_rollup`)
				require.NoError(t, err)
			})
			for _, sample := range []struct{ id, successes, failures, ttft int64 }{
				{active, 2, 1, 100}, {deleted, 3, 2, 500}, {disabled, 5, 3, 1000},
			} {
				_, err := db.ExecContext(ctx, `
					INSERT INTO channel_monitor_v2_metrics_rollup (bucket_start, bucket_seconds, platform, model, group_id,
					    success_requests, error_requests, input_tokens, cache_read_tokens, ttft_sum_ms, ttft_count)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $6)`,
					bucketStart, int(filter.Bucket.Seconds()), platform, model, sample.id, sample.successes, sample.failures,
					sample.successes*10, sample.successes*5, sample.successes*sample.ttft)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `
					INSERT INTO channel_monitor_v2_latency_histograms_rollup (bucket_start, bucket_seconds, platform, model,
					    group_id, metric, upper_bound_ms, sample_count)
					VALUES ($1, $2, $3, $4, $5, 'ttft', $6, $7)`,
					bucketStart, int(filter.Bucket.Seconds()), platform, model, sample.id, sample.ttft, sample.successes)
				require.NoError(t, err)
			}

			readMatrix := func(scope service.ChannelMonitorV2Filter, grouping service.ChannelMonitorV2GroupBy) []service.ChannelMonitorV2MatrixRow {
				matrix, err := repo.GetMatrix(ctx, scope, cfg, grouping, true)
				require.NoError(t, err)
				return matrix.Items
			}
			before, err := repo.GetDimensions(ctx, filter, cfg)
			require.NoError(t, err)
			require.Len(t, before.Groups, 3)
			require.Len(t, before.Platforms, 1)
			require.Len(t, before.Models, 1)
			require.Equal(t, int64(16), before.Platforms[0].RequestCount)
			require.Equal(t, int64(16), before.Models[0].RequestCount)
			platformRows := readMatrix(filter, service.ChannelMonitorV2GroupByPlatform)
			modelRows := readMatrix(filter, service.ChannelMonitorV2GroupByPlatformModel)
			models, err := repo.GetModels(ctx, filter, cfg, true)
			require.NoError(t, err)
			snapshot, err := repo.GetSnapshot(ctx, filter, cfg, true)
			require.NoError(t, err)
			require.Equal(t, int64(16), snapshot.Metrics.RequestCount)
			require.NotNil(t, snapshot.Metrics.TTFT.P95Ms)
			require.Equal(t, int64(1000), *snapshot.Metrics.TTFT.P95Ms)
			activeFilter := filter
			activeFilter.GroupIDs = []int64{active}
			activeRows := readMatrix(activeFilter, service.ChannelMonitorV2GroupByPlatformGroup)
			activeModelRows := readMatrix(activeFilter, service.ChannelMonitorV2GroupByPlatformGroupModel)

			_, err = db.ExecContext(ctx, `UPDATE groups SET deleted_at = NOW() WHERE id = $1`, deleted)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE groups SET status = 'disabled' WHERE id = $1`, disabled)
			require.NoError(t, err)

			after, err := repo.GetDimensions(ctx, filter, cfg)
			require.NoError(t, err)
			assert.Equal(t, []service.ChannelMonitorV2GroupDimension{
				{ID: active, Name: activeName, Platform: platform, RequestCount: 3},
			}, after.Groups)
			require.Equal(t, before.Platforms, after.Platforms)
			require.Equal(t, before.Models, after.Models)
			assert.Equal(t, activeRows, readMatrix(filter, service.ChannelMonitorV2GroupByPlatformGroup))
			assert.Equal(t, activeModelRows, readMatrix(filter, service.ChannelMonitorV2GroupByPlatformGroupModel))
			require.Equal(t, activeRows, readMatrix(activeFilter, service.ChannelMonitorV2GroupByPlatformGroup))
			require.Equal(t, platformRows, readMatrix(filter, service.ChannelMonitorV2GroupByPlatform))
			require.Equal(t, modelRows, readMatrix(filter, service.ChannelMonitorV2GroupByPlatformModel))
			currentModels, err := repo.GetModels(ctx, filter, cfg, true)
			require.NoError(t, err)
			require.Equal(t, models.Items, currentModels.Items)
			current, err := repo.GetSnapshot(ctx, filter, cfg, true)
			require.NoError(t, err)
			require.Equal(t, snapshot.Metrics, current.Metrics)
			require.Equal(t, snapshot.Trend, current.Trend)

			replacement := createGroup(deletedName)
			require.NotEqual(t, deleted, replacement)
			rows := readMatrix(filter, service.ChannelMonitorV2GroupByPlatformGroup)
			require.Len(t, rows, 2)
			require.Equal(t, activeRows[0], rows[0])
			require.Equal(t, &replacement, rows[1].GroupID)
			require.Equal(t, deletedName, rows[1].GroupName)
			require.Zero(t, rows[1].Metrics.RequestCount)
			require.Empty(t, rows[1].Buckets)
		})
	}
}

func channelMonitorV2VisibilityDB(t *testing.T) *sql.DB {
	t.Helper()
	const schema = "monitor_v2_group_visibility"
	_, err := integrationDB.ExecContext(t.Context(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	})
	dsn, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	// Use the migrated table and index definitions with a private watermark and
	// group catalog so this fixture cannot affect other repository tests.
	for _, table := range []string{
		"groups", "channel_monitor_v2_watermarks", "channel_monitor_v2_metrics_rollup",
		"channel_monitor_v2_latency_histograms_rollup", "channel_monitor_v2_error_metrics_rollup",
	} {
		_, err := db.ExecContext(t.Context(), "CREATE TABLE "+table+" (LIKE public."+table+" INCLUDING ALL)")
		require.NoError(t, err)
	}
	// Copying a serial default would continue to consume IDs from public.groups.
	_, err = db.ExecContext(t.Context(), `
		ALTER TABLE groups ALTER COLUMN id DROP DEFAULT;
		ALTER TABLE groups ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY`)
	require.NoError(t, err)
	return db
}
