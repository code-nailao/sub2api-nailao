package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeepSeekAuthCachePolicyRoundTrip(t *testing.T) {
	groupID, accountID, price := int64(218), int64(7181), 6.0
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, Key: "fixture-only", Status: StatusActive, DeepSeekToolMode: "disabled", User: &User{ID: 3, Status: StatusActive}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, AccountID: &accountID, FetchPricePer1K: &price}}}
	svc := &APIKeyService{}
	wire, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
	require.NoError(t, err)
	var entry APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(wire, &entry))
	got, used, err := svc.applyAuthCacheEntry(key.Key, &entry)
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, "disabled", got.DeepSeekToolMode)
	require.Equal(t, key.Group.DeepSeekTools, got.Group.DeepSeekTools)
	require.False(t, got.DeepSeekToolsAutoInject())
}

type deepSeekAuthInvalidationFixture struct {
	key    string
	userID int64
}

func (f *deepSeekAuthInvalidationFixture) InvalidateAuthCacheByKey(_ context.Context, key string) {
	f.key = key
}
func (f *deepSeekAuthInvalidationFixture) InvalidateAuthCacheByUserID(_ context.Context, id int64) {
	f.userID = id
}

func TestDeepSeekSettledQuotaInvalidatesLiveAndRecoveryAuth(t *testing.T) {
	ctx := context.Background()
	live := &deepSeekAuthInvalidationFixture{}
	invalidateDeepSeekAuthCache(ctx, &APIKey{Key: "fixture-only", UserID: 3}, &UsageBillingApplyResult{Applied: true, APIKeyQuotaExhausted: true}, live)
	require.Equal(t, "fixture-only", live.key)
	require.Zero(t, live.userID)
	recovery := &deepSeekAuthInvalidationFixture{}
	invalidateDeepSeekAuthCache(ctx, &APIKey{ID: 2, UserID: 3}, &UsageBillingApplyResult{Applied: true, APIKeyQuotaExhausted: true}, recovery)
	require.EqualValues(t, 3, recovery.userID)
	require.Empty(t, recovery.key, "recovery needs no credential in the journal")
	replay := &deepSeekAuthInvalidationFixture{}
	invalidateDeepSeekAuthCache(ctx, &APIKey{ID: 2, UserID: 3}, &UsageBillingApplyResult{Applied: false}, replay)
	require.EqualValues(t, 3, replay.userID, "an ambiguous committed settlement still refreshes auth")
}
