//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type usageGroupRepo struct {
	ChannelMonitorRepository
	keys     []string
	deadline time.Time
	groups   map[string]int64
	err      error
}

func (r *usageGroupRepo) ResolveUsageGroupsByAPIKeys(ctx context.Context, keys []string) (map[string]int64, error) {
	r.keys = append([]string(nil), keys...)
	r.deadline, _ = ctx.Deadline()
	return r.groups, r.err
}
func TestMonitorUsageGroupsFilterDeduplicateAndRefresh(t *testing.T) {
	repo := &usageGroupRepo{groups: map[string]int64{"local": 246, "replacement": 249, "broken": 500, "quota": 600}}
	svc := NewChannelMonitorService(repo, nil)
	monitors := []*ChannelMonitor{
		{ID: 104, APIKey: "local"}, {ID: 105, APIKey: "local"}, {ID: 106, APIKey: "external"},
		{ID: 107, APIKey: "broken", APIKeyDecryptFailed: true}, {ID: 108}, {ID: 109, APIKey: "quota", CheckMode: MonitorCheckModeQuota},
		{ID: 110, APIKey: "local", ExtraHeaders: map[string]string{"aUtHoRiZaTiOn": "Bearer another-key"}},
		{ID: 111, APIKey: "local", ExtraHeaders: map[string]string{"X-Api-Key": "another-key"}},
		{ID: 112, APIKey: "local", ExtraHeaders: map[string]string{"x-goog-api-key": "another-key"}},
		{ID: 113, APIKey: "local", ExtraHeaders: map[string]string{"api-key": "another-key"}},
	}
	before := time.Now()
	got, err := svc.ResolveUsageGroups(context.Background(), monitors)
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{104: 246, 105: 246}, got)
	require.ElementsMatch(t, []string{"local", "external"}, repo.keys)
	require.WithinDuration(t, before.Add(time.Second), repo.deadline, 100*time.Millisecond)
	// Replacing a monitor key and rebinding an existing key are both live reads.
	monitors[0].APIKey = "replacement"
	repo.groups["local"] = 250
	got, err = svc.ResolveUsageGroups(context.Background(), monitors)
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{104: 249, 105: 250}, got)
	repo.err = errors.New("database failure")
	got, err = svc.ResolveUsageGroups(context.Background(), monitors)
	require.Error(t, err)
	require.Empty(t, got, "failed lookup must not retain a prior association")
	repo.keys = nil
	got, err = svc.ResolveUsageGroups(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Nil(t, repo.keys)
}
