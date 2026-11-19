//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMonitorUsageGroupMembershipTracksMovesAndDeletion(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	groupA := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("monitor-usage-a-%d", suffix), Platform: service.PlatformOpenAI, RateMultiplier: 1})
	groupB := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("monitor-usage-b-%d", suffix), Platform: service.PlatformOpenAI, RateMultiplier: 1})
	user := mustCreateUser(t, integrationEntClient, &service.User{Email: fmt.Sprintf("monitor-usage-%d@example.com", suffix), Concurrency: 1})
	keyValue := fmt.Sprintf("sk-monitor-usage-%d", suffix)
	keys := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupA.ID, Key: keyValue, Name: "monitor-usage", Status: service.StatusActive}
	require.NoError(t, keys.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id=$1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id IN ($1,$2)", groupA.ID, groupB.ID)
		require.NoError(t, err)
	})
	repo := NewChannelMonitorRepository(integrationEntClient, integrationDB)
	check := func(want int64) {
		t.Helper()
		got, err := repo.ResolveUsageGroupsByAPIKeys(ctx, []string{keyValue, keyValue, "unknown-key"})
		require.NoError(t, err)
		if want == 0 {
			require.Empty(t, got)
		} else {
			require.Equal(t, map[string]int64{keyValue: want}, got)
		}
	}
	check(groupA.ID)
	_, err := integrationDB.ExecContext(ctx, "UPDATE api_keys SET group_id=$1 WHERE id=$2", groupB.ID, key.ID)
	require.NoError(t, err)
	check(groupB.ID)
	_, err = integrationDB.ExecContext(ctx, "UPDATE api_keys SET status='inactive' WHERE id=$1", key.ID)
	require.NoError(t, err)
	check(groupB.ID)
	_, err = integrationDB.ExecContext(ctx, "UPDATE api_keys SET group_id=NULL WHERE id=$1", key.ID)
	require.NoError(t, err)
	check(0)
	_, err = integrationDB.ExecContext(ctx, "UPDATE api_keys SET group_id=$1, deleted_at=NOW() WHERE id=$2", groupA.ID, key.ID)
	require.NoError(t, err)
	check(0)
	_, err = integrationDB.ExecContext(ctx, "UPDATE api_keys SET deleted_at=NULL WHERE id=$1", key.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "UPDATE groups SET deleted_at=NOW() WHERE id=$1", groupA.ID)
	require.NoError(t, err)
	check(0)
	got, err := repo.ResolveUsageGroupsByAPIKeys(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, got)
}
