//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2ImageGroupingPostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := NewChannelMonitorV2Repository(integrationDB)
	svc := service.NewChannelMonitorV2Service(repo)
	actor := mustCreateUser(t, integrationEntClient, &service.User{Role: service.RoleAdmin})
	original, err := repo.GetConfig(ctx)
	require.NoError(t, err)
	var previousOrder string
	readErr := integrationDB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", channelMonitorV2DisplayOrderSettingKey).Scan(&previousOrder)
	require.True(t, readErr == nil || errors.Is(readErr, sql.ErrNoRows))
	t.Cleanup(func() {
		latest, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		_, restoreErr := repo.UpdateConfig(ctx, *original, latest.Version)
		require.NoError(t, restoreErr)
		var cleanupErr error
		if errors.Is(readErr, sql.ErrNoRows) {
			_, cleanupErr = integrationDB.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", channelMonitorV2DisplayOrderSettingKey)
		} else {
			_, cleanupErr = integrationDB.ExecContext(ctx, "UPDATE settings SET value=$2 WHERE key=$1", channelMonitorV2DisplayOrderSettingKey, previousOrder)
		}
		require.NoError(t, cleanupErr)
		_, cleanupErr = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", actor.ID)
		require.NoError(t, cleanupErr)
	})

	current := *original
	for _, mode := range []string{"", "separate", "provider"} {
		name := mode
		if name == "" {
			name = "legacy default"
		}
		t.Run(name, func(t *testing.T) {
			current.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{
				Categories: []string{"openai", "anthropic", "grok", "gemini", "domestic", "image", "other"},
				Groups: map[string][]int64{
					"openai": {246}, "anthropic": {48}, "grok": {132}, "gemini": {59},
					"domestic": {216}, "image": {74, 121}, "other": {},
				},
				GroupCategories: map[string]string{"121": "openai", "74": "image"},
				ImageGrouping:   mode,
			}
			updated, saveErr := svc.UpdateConfig(ctx, current, current.Version, actor.ID)
			require.NoError(t, saveErr)
			reloaded, loadErr := repo.GetConfig(ctx)
			require.NoError(t, loadErr)
			require.Equal(t, updated.Version, reloaded.Version)
			require.Equal(t, current.DisplayOrder, reloaded.DisplayOrder)
			current = *reloaded
		})
	}
	t.Run("old client omission retains provider preference", func(t *testing.T) {
		current.DisplayOrder = nil
		updated, saveErr := svc.UpdateConfig(ctx, current, current.Version, actor.ID)
		require.NoError(t, saveErr)
		require.Equal(t, "provider", updated.DisplayOrder.ImageGrouping)
		require.Equal(t, "openai", updated.DisplayOrder.GroupCategories["121"])
	})
}

func TestChannelMonitorV2DisplayOrderPostgresTransactions(t *testing.T) {
	ctx := context.Background()
	repo := NewChannelMonitorV2Repository(integrationDB)
	svc := service.NewChannelMonitorV2Service(repo)
	actor := mustCreateUser(t, integrationEntClient, &service.User{Role: service.RoleAdmin})
	original, err := repo.GetConfig(ctx)
	require.NoError(t, err)
	var previousOrder string
	readErr := integrationDB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", channelMonitorV2DisplayOrderSettingKey).Scan(&previousOrder)
	require.True(t, readErr == nil || errors.Is(readErr, sql.ErrNoRows))
	t.Cleanup(func() {
		_, cleanupErr := integrationDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS monitor_display_order_reject ON settings; DROP FUNCTION IF EXISTS monitor_display_order_reject()`)
		require.NoError(t, cleanupErr)
		latest, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		_, restoreErr := repo.UpdateConfig(ctx, *original, latest.Version)
		require.NoError(t, restoreErr)
		if errors.Is(readErr, sql.ErrNoRows) {
			_, cleanupErr = integrationDB.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", channelMonitorV2DisplayOrderSettingKey)
		} else {
			_, cleanupErr = integrationDB.ExecContext(ctx, "UPDATE settings SET value=$2 WHERE key=$1", channelMonitorV2DisplayOrderSettingKey, previousOrder)
		}
		require.NoError(t, cleanupErr)
		_, cleanupErr = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", actor.ID)
		require.NoError(t, cleanupErr)
	})

	order := &service.ChannelMonitorV2DisplayOrder{
		Categories:      []string{"other", "domestic", "grok", "openai", "anthropic", "gemini"},
		Groups:          map[string][]int64{"other": {121, 74}, "domestic": {224, 216}, "openai": {155, 246, 8}},
		GroupCategories: map[string]string{"121": "other", "74": "other", "216": "domestic", "224": "domestic"},
	}
	current := *original
	t.Run("writes order and version together", func(t *testing.T) {
		current.DisplayOrder = order
		updated, saveErr := svc.UpdateConfig(ctx, current, current.Version, actor.ID)
		require.NoError(t, saveErr)
		require.Equal(t, current.Version+1, updated.Version)
		require.Equal(t, order, updated.DisplayOrder)
		reloaded, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		require.Equal(t, updated.Version, reloaded.Version)
		require.Equal(t, order, reloaded.DisplayOrder)
		current = *reloaded
	})
	t.Run("stale version cannot overwrite saved order", func(t *testing.T) {
		stale := current
		stale.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{}
		_, saveErr := svc.UpdateConfig(ctx, stale, current.Version-1, actor.ID)
		require.ErrorIs(t, saveErr, service.ErrChannelMonitorV2ConfigConflict)
		reloaded, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		require.Equal(t, current.Version, reloaded.Version)
		require.Equal(t, order, reloaded.DisplayOrder)
	})
	t.Run("legacy client omission preserves order", func(t *testing.T) {
		legacy := current
		legacy.DisplayOrder = nil
		legacy.RefreshIntervalSeconds = 60
		updated, saveErr := svc.UpdateConfig(ctx, legacy, current.Version, actor.ID)
		require.NoError(t, saveErr)
		require.Equal(t, order, updated.DisplayOrder)
		require.Equal(t, 60, updated.RefreshIntervalSeconds)
		current = *updated
	})
	t.Run("failed settings write rolls back config version", func(t *testing.T) {
		_, triggerErr := integrationDB.ExecContext(ctx, `
			CREATE FUNCTION monitor_display_order_reject() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.key = 'sources_channel_monitor_v2_display_order' THEN
					RAISE EXCEPTION 'deliberate monitor display order failure';
				END IF;
				RETURN NEW;
			END $$;
			CREATE TRIGGER monitor_display_order_reject BEFORE INSERT OR UPDATE ON settings
			FOR EACH ROW EXECUTE FUNCTION monitor_display_order_reject()`)
		require.NoError(t, triggerErr)
		rejected := current
		rejected.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{}
		rejected.RefreshIntervalSeconds = 300
		_, saveErr := svc.UpdateConfig(ctx, rejected, current.Version, actor.ID)
		require.ErrorContains(t, saveErr, "deliberate monitor display order failure")
		reloaded, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		require.Equal(t, current.Version, reloaded.Version)
		require.Equal(t, current.RefreshIntervalSeconds, reloaded.RefreshIntervalSeconds)
		require.Equal(t, order, reloaded.DisplayOrder)
		_, triggerErr = integrationDB.ExecContext(ctx, `DROP TRIGGER monitor_display_order_reject ON settings; DROP FUNCTION monitor_display_order_reject()`)
		require.NoError(t, triggerErr)
	})
	t.Run("concurrent editors have exactly one winner", func(t *testing.T) {
		first, second := current, current
		first.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{Categories: []string{"grok", "openai"}}
		second.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{Categories: []string{"domestic", "gemini"}}
		type result struct {
			config *service.ChannelMonitorV2Config
			err    error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		for _, input := range []service.ChannelMonitorV2Config{first, second} {
			go func(cfg service.ChannelMonitorV2Config) {
				ready.Done()
				<-start
				updated, saveErr := svc.UpdateConfig(ctx, cfg, cfg.Version, actor.ID)
				results <- result{updated, saveErr}
			}(input)
		}
		ready.Wait()
		close(start)
		var winner *service.ChannelMonitorV2Config
		conflicts := 0
		for i := 0; i < 2; i++ {
			result := <-results
			if result.err == nil {
				require.Nil(t, winner, "only one editor may win")
				winner = result.config
			} else {
				require.ErrorIs(t, result.err, service.ErrChannelMonitorV2ConfigConflict)
				conflicts++
			}
		}
		require.NotNil(t, winner)
		require.Equal(t, 1, conflicts)
		reloaded, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		require.Equal(t, current.Version+1, reloaded.Version)
		require.Equal(t, winner.DisplayOrder, reloaded.DisplayOrder)
		current = *reloaded
	})
	t.Run("empty object resets order and survives reread", func(t *testing.T) {
		current.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{}
		updated, saveErr := svc.UpdateConfig(ctx, current, current.Version, actor.ID)
		require.NoError(t, saveErr)
		reloaded, loadErr := repo.GetConfig(ctx)
		require.NoError(t, loadErr)
		require.Equal(t, updated.Version, reloaded.Version)
		require.NotNil(t, reloaded.DisplayOrder)
		require.Empty(t, reloaded.DisplayOrder.Categories)
		require.Empty(t, reloaded.DisplayOrder.Groups)
		require.Empty(t, reloaded.DisplayOrder.GroupCategories)
	})
}
