package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeRequestSpeedValidationAndCacheRoundtrip(t *testing.T) {
	ordinary := &Group{Platform: PlatformAnthropic}
	require.NoError(t, ValidateGroupRequestSpeed(ordinary))
	require.False(t, ordinary.RequestSpeedSelectionEnabled)
	require.Equal(t, RequestSpeedUltrafast, ordinary.DefaultRequestSpeedTier)
	ordinary.RequestSpeedSelectionEnabled = true
	require.Error(t, ValidateGroupRequestSpeed(ordinary))
	tiers, err := NormalizeRequestSpeedTiers([]string{"fast"})
	require.NoError(t, err)
	require.Equal(t, []string{"fast"}, tiers)
	_, err = NormalizeRequestSpeedTiers([]string{"fast", "ultrafast"})
	require.ErrorIs(t, err, ErrRequestSpeedInvalid)
	_, err = NormalizeRequestSpeedTiers([]string{"fast", "fast"})
	require.ErrorIs(t, err, ErrRequestSpeedInvalid)
	_, err = NormalizeRequestSpeedTiers([]string{"priority"})
	require.ErrorIs(t, err, ErrRequestSpeedInvalid)

	groupID := int64(55)
	fast := RequestSpeedFast
	key := &APIKey{ID: 7, UserID: 1, Key: "local-only", GroupID: &groupID,
		RequestSpeedTier: &fast, DeepSeekToolMode: "inherit", Status: StatusActive,
		User: &User{ID: 1, Status: StatusActive},
		Group: &Group{ID: groupID, Hydrated: true, Platform: PlatformOpenAI, Status: StatusActive,
			RequestSpeedSelectionEnabled: true, DefaultRequestSpeedTier: RequestSpeedUltrafast,
			ForceOpenAIUltrafast: true, OpenAIServiceTierPolicy: "force_ultrafast"}}
	s := &APIKeyService{}
	raw, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: s.snapshotFromAPIKey(context.Background(), key)})
	require.NoError(t, err)
	var cached APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(raw, &cached))
	loaded, used, err := s.applyAuthCacheEntry(key.Key, &cached)
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, fast, *loaded.RequestSpeedTier)
	require.True(t, loaded.Group.RequestSpeedSelectionEnabled)
	require.Equal(t, RequestSpeedUltrafast, loaded.Group.DefaultRequestSpeedTier)
	require.Equal(t, "force_ultrafast", loaded.Group.OpenAIServiceTierPolicy)
	require.True(t, loaded.Group.ForceOpenAIUltrafast)
	cached.Snapshot.Version = 29
	_, used, err = s.applyAuthCacheEntry(key.Key, &cached)
	require.NoError(t, err)
	require.False(t, used, "pre-speed snapshot must not grant stale routing")
}
