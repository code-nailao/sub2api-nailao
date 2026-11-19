//go:build integration

package repository

import (
	"context"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorSortPersistsAcrossReadsAndPreservesEdits(t *testing.T) {
	ctx := context.Background()
	repo := NewChannelMonitorRepository(integrationEntClient, integrationDB)
	create := func(name string, enabled bool) *service.ChannelMonitor {
		m := &service.ChannelMonitor{Name: "order-regression-" + name, Provider: "openai", APIMode: "chat_completions", Endpoint: "https://api.example.com", APIKey: "encrypted-placeholder", PrimaryModel: "gpt-test", Enabled: enabled, IntervalSeconds: 60, CheckMode: "probe", BodyOverrideMode: "off", CreatedBy: 1}
		require.NoError(t, repo.Create(ctx, m))
		t.Cleanup(func() {
			_, err := integrationDB.ExecContext(ctx, "DELETE FROM channel_monitors WHERE id=$1", m.ID)
			require.NoError(t, err)
		})
		return m
	}
	a, b, disabled := create("a", true), create("b", true), create("disabled", false)
	require.Greater(t, b.SortOrder, a.SortOrder)
	require.NoError(t, repo.UpdateSortOrders(ctx, []service.ChannelMonitorSortOrderUpdate{{ID: b.ID, SortOrder: 0}, {ID: disabled.ID, SortOrder: 10}, {ID: a.ID, SortOrder: 20}}))
	// A stale edit must not overwrite an ordering saved after the row was read.
	a.Name = "order-regression-renamed"
	require.NoError(t, repo.Update(ctx, a))
	loaded, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 20, loaded.SortOrder)
	items, total, err := repo.List(ctx, service.ChannelMonitorListParams{Page: 1, PageSize: 2, Search: "order-regression-"})
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	require.Equal(t, []int64{b.ID, disabled.ID}, []int64{items[0].ID, items[1].ID})
	items, _, err = repo.List(ctx, service.ChannelMonitorListParams{Page: 2, PageSize: 2, Search: "order-regression-"})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, a.ID, items[0].ID)
	enabled, err := repo.ListEnabled(ctx)
	require.NoError(t, err)
	var enabledIDs []int64
	for _, item := range enabled {
		if item.ID == a.ID || item.ID == b.ID || item.ID == disabled.ID {
			enabledIDs = append(enabledIDs, item.ID)
		}
	}
	require.Equal(t, []int64{b.ID, a.ID}, enabledIDs)
	// A deleted target must reject the whole batch, including the still-valid row.
	require.NoError(t, repo.Delete(ctx, disabled.ID))
	err = repo.UpdateSortOrders(ctx, []service.ChannelMonitorSortOrderUpdate{{ID: b.ID, SortOrder: 999}, {ID: disabled.ID, SortOrder: 1000}})
	require.ErrorIs(t, err, service.ErrChannelMonitorNotFound)
	loaded, err = repo.GetByID(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 0, loaded.SortOrder)
	// The cleanup is deliberately idempotent for the deleted fixture above.
}

func TestChannelMonitorSortMigrationPreservesLegacyOrBackfillsFresh(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/246_sources_channel_monitor_sort_order.sql")
	require.NoError(t, err)
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			conn, err := integrationDB.Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			schema := "test_monitor_order_" + name
			_, err = conn.ExecContext(ctx, "CREATE SCHEMA "+schema)
			require.NoError(t, err)
			defer func() { _, _ = conn.ExecContext(ctx, "RESET search_path; DROP SCHEMA "+schema+" CASCADE") }()
			_, err = conn.ExecContext(ctx, "SET search_path TO "+schema+"; CREATE TABLE channel_monitors (id BIGINT PRIMARY KEY)")
			require.NoError(t, err)
			if legacy {
				_, err = conn.ExecContext(ctx, "ALTER TABLE channel_monitors ADD COLUMN sort_order INT NOT NULL DEFAULT 0; INSERT INTO channel_monitors VALUES (1,70),(2,-10),(3,20)")
			} else {
				_, err = conn.ExecContext(ctx, "INSERT INTO channel_monitors VALUES (1),(2),(3)")
			}
			require.NoError(t, err)
			for i := 0; i < 2; i++ {
				_, err = conn.ExecContext(ctx, string(migration))
				require.NoError(t, err)
				var positions string
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT string_agg(sort_order::text, ',' ORDER BY id) FROM channel_monitors").Scan(&positions))
				want := "20,10,0"
				if legacy {
					want = "70,-10,20"
				}
				require.Equal(t, want, positions)
			}
		})
	}
}
