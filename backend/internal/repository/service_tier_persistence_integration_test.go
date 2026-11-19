//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestServiceTierGroupPersistence(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newGroupRepositoryWithSQL(tx.Client(), tx)
	fast, ultra := 2.5, 8.0
	group := &service.Group{
		Name: fmt.Sprintf("tier-group-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI,
		RateMultiplier: .12, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		OpenAIServiceTierPolicy: service.GroupTierForceUltrafast,
		ModelPricing:            []service.ChannelModelPricing{{Models: []string{"gpt-6.1-sol"}, FastMultiplier: &fast, UltrafastMultiplier: &ultra}},
	}
	require.NoError(t, repo.Create(ctx, group))
	got, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, service.GroupTierForceUltrafast, got.OpenAIServiceTierPolicy)
	require.Equal(t, 8.0, *got.ModelPricing[0].UltrafastMultiplier)
	got.OpenAIServiceTierPolicy = service.GroupTierPassthrough
	require.NoError(t, repo.Update(ctx, got))
	readBack, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, service.GroupTierPassthrough, readBack.OpenAIServiceTierPolicy)
	require.Equal(t, .12, readBack.RateMultiplier)
}

func TestUsageBillingSnapshotPersistsAndDuplicateDoesNotRewrite(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "tier-" + uuid.NewString() + "@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "test-tier-" + uuid.NewString(), Name: "tier"})
	account := mustCreateAccount(t, client, &service.Account{Name: "tier-" + uuid.NewString()})
	requested := "default"
	snapshot := &service.UsageBillingSnapshot{Version: 1, RequestedTier: &requested,
		ForwardedTier: "ultrafast", ObservedTier: "ultrafast", BilledTier: "ultrafast",
		TierMultiplier: 6, ChargedTierMultiplier: 6, GroupMultiplier: .12, EffectiveRateMultiplier: .12,
		EffectiveUnitPrices: map[string]float64{"input": 12e-6, "output": 60e-6},
	}
	log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
		RequestID: uuid.NewString(), Model: "gpt-6.1-sol", InputTokens: 1000, OutputTokens: 1000,
		TotalCost: .072, ActualCost: .00864, BillingSnapshot: snapshot,
	}
	inserted, err := repo.Create(ctx, log)
	require.NoError(t, err)
	require.True(t, inserted)
	got, err := repo.GetByID(ctx, log.ID)
	require.NoError(t, err)
	require.Equal(t, snapshot, got.BillingSnapshot)
	duplicate := *log
	changed := *snapshot
	changed.TierMultiplier = 99
	duplicate.BillingSnapshot = &changed
	inserted, err = repo.Create(ctx, &duplicate)
	require.NoError(t, err)
	require.False(t, inserted)
	got, err = repo.GetByID(ctx, log.ID)
	require.NoError(t, err)
	require.Equal(t, 6.0, got.BillingSnapshot.TierMultiplier)
}

func TestServiceTierChannelPricingPersistence(t *testing.T) {
	ctx := context.Background()
	repo := &channelRepository{db: integrationDB}
	channel := &service.Channel{Name: "tier-" + uuid.NewString(), Status: service.StatusActive,
		BillingModelSource: service.BillingModelSourceChannelMapped}
	require.NoError(t, repo.Create(ctx, channel))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, "DELETE FROM channels WHERE id = $1", channel.ID) })
	fast, ultra := 2.5, 8.0
	pricing := &service.ChannelModelPricing{ChannelID: channel.ID, Platform: service.PlatformOpenAI,
		Models: []string{"gpt-6.1-sol"}, FastMultiplier: &fast, UltrafastMultiplier: &ultra}
	require.NoError(t, repo.CreateModelPricing(ctx, pricing))
	got, err := repo.GetByID(ctx, channel.ID)
	require.NoError(t, err)
	require.Len(t, got.ModelPricing, 1)
	require.Equal(t, 8.0, *got.ModelPricing[0].UltrafastMultiplier)
	require.Nil(t, got.ModelPricing[0].InputPrice, "tier-only cards must inherit standard prices")
	ultra = 6
	require.NoError(t, repo.UpdateModelPricing(ctx, pricing))
	batch, err := repo.batchLoadModelPricing(ctx, []int64{channel.ID})
	require.NoError(t, err)
	require.Equal(t, 6.0, *batch[channel.ID][0].UltrafastMultiplier)
	pricing.UltrafastMultiplier = nil
	require.NoError(t, repo.UpdateModelPricing(ctx, pricing))
	got, err = repo.GetByID(ctx, channel.ID)
	require.NoError(t, err)
	require.Nil(t, got.ModelPricing[0].UltrafastMultiplier)
}
