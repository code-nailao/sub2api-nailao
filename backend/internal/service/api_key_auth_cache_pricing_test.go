package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotGroupPricingRoundtrip(t *testing.T) {
	groupID := int64(50)
	inputPrice := 1e-6
	outputPrice := 2e-6
	apiKey := &APIKey{
		ID: 82, UserID: 40, GroupID: &groupID, Key: "sk-pricing-roundtrip", Status: StatusActive,
		User: &User{ID: 40, Status: StatusActive},
		Group: &Group{
			ID: groupID, Name: "pricing-roundtrip", Platform: PlatformAnthropic, Status: StatusActive,
			LongContextPricingEnabled: true,
			ModelPricing: []ChannelModelPricing{{
				Models: []string{"claude-sonnet-*"}, BillingMode: BillingModeToken,
				InputPrice: &inputPrice, OutputPrice: &outputPrice,
			}},
		},
	}
	svc := &APIKeyService{}

	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), apiKey)})
	require.NoError(t, err)
	var cached APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &cached))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &cached)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)
	require.True(t, materialized.Group.LongContextPricingEnabled)
	require.Equal(t, apiKey.Group.ModelPricing, materialized.Group.ModelPricing)

	billing := &BillingService{fallbackPrices: map[string]*ModelPricing{
		"claude-sonnet-4": {InputPricePerToken: 3e-6, OutputPricePerToken: 15e-6},
	}}
	resolver := NewModelPricingResolver(nil, billing)
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "claude-sonnet-4", Group: materialized.Group})
	require.Equal(t, PricingSourceGroup, resolved.Source)
	require.True(t, resolved.longContextPricingEnabled)
	require.InDelta(t, inputPrice, resolved.BasePricing.InputPricePerToken, 1e-12)
	require.InDelta(t, outputPrice, resolved.BasePricing.OutputPricePerToken, 1e-12)
}

func TestAPIKeyAuthSnapshotServiceTierRoundtrip(t *testing.T) {
	groupID := int64(226)
	fast, ultra := 2.5, 8.0
	key := &APIKey{ID: 81, UserID: 40, GroupID: &groupID, Key: "test-tier-cache", Status: StatusActive,
		User: &User{ID: 40, Status: StatusActive}, Group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive,
			OpenAIServiceTierPolicy: GroupTierForceUltrafast, RateMultiplier: .12,
			ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-6.1-sol"}, FastMultiplier: &fast, UltrafastMultiplier: &ultra}},
		},
	}
	svc := &APIKeyService{}
	data, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
	require.NoError(t, err)
	var cached APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(data, &cached))
	materialized, used, err := svc.applyAuthCacheEntry(key.Key, &cached)
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, GroupTierForceUltrafast, materialized.Group.OpenAIServiceTierPolicy)
	require.Equal(t, .12, materialized.Group.RateMultiplier)
	require.Equal(t, 2.5, *materialized.Group.ModelPricing[0].FastMultiplier)
	require.Equal(t, 8.0, *materialized.Group.ModelPricing[0].UltrafastMultiplier)
}

// A stable pre-tier instance can repopulate shared Redis while the candidate is
// serving. Its v24 snapshot cannot represent either the group policy or Ultra
// override. It must be a cache miss, even when all older fields are valid.
func TestAPIKeyAuthSnapshotRejectsV24MissingServiceTierFields(t *testing.T) {
	groupID := int64(226)
	fast := 2.5
	old := &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{
		Version: 24, APIKeyID: 81, UserID: 40, GroupID: &groupID, Status: StatusActive,
		User: APIKeyAuthUserSnapshot{ID: 40, Status: StatusActive, Role: RoleUser, Balance: 10, Concurrency: 1},
		Group: &APIKeyAuthGroupSnapshot{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive,
			SubscriptionType: SubscriptionTypeStandard, RateMultiplier: .12,
			ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-6.1-sol"}, FastMultiplier: &fast}},
		},
	}}
	svc := &APIKeyService{}
	materialized, used, err := svc.applyAuthCacheEntry("old-writer-tier-key", old)
	require.NoError(t, err)
	require.False(t, used, "old writer must not erase the new policy or subscription Ultra 8x")
	require.Nil(t, materialized)
}
