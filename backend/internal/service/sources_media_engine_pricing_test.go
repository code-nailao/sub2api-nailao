//go:build unit

package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestSourcesMediaEngineUsesNativePricingPriority(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "gemini-3-pro-image"} {
		for _, which := range []string{"channel", "group-image", "group-model"} {
			t.Run(model+"/"+which, func(t *testing.T) {
				gid := int64(74)
				price := .2
				groupPrice := .1
				modelPrice := .4
				g := newOpenAIRecordUsageServiceForTest(&openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				g.resolver = newOpenAIImageChannelPricingResolverForTest(t, gid, model, price)
				group := &Group{ID: gid, Platform: PlatformOpenAI, Hydrated: true, RateMultiplier: 2}
				want := price
				if which != "channel" {
					group.ImagePrice2K = &groupPrice
					want = groupPrice
				}
				if which == "group-model" {
					group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeImage, PerRequestPrice: &modelPrice}}
					want = modelPrice
				}
				key := &APIKey{ID: 2, UserID: 1, User: &User{ID: 1}, GroupID: &gid, Group: group}
				svc := &SourcesMediaEngine{gateway: g}
				ticket, err := svc.quote(context.Background(), key, &Account{ID: 713, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}, &sourcesMediaEngineRequest{Model: model, Size: "2K", Quality: "high"}, "/v1/images/generations/async", "", "")
				require.NoError(t, err)
				require.InDelta(t, want*2, ticket.Amount, 1e-9)
				require.Equal(t, PlatformOpenAI, ticket.Platform)
				require.Zero(t, ticket.Billing.BalanceCost)
			})
		}
	}
}

type sourcesQuotaCache struct{ fakeIncrCache }

func (c *sourcesQuotaCache) GetUserPlatformQuotaCache(context.Context, int64, string) (*UserPlatformQuotaCacheEntry, bool, error) {
	limit := 100.0
	return &UserPlatformQuotaCacheEntry{DailyLimitUSD: &limit}, true, nil
}
func TestSourcesHeldBalancePostEffectsUseNativeQuotaWithoutSecondDeduction(t *testing.T) {
	cache := &sourcesQuotaCache{}
	writes := make(chan cacheWriteTask, 4)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Billing.UserPlatformQuotaCacheTTLSeconds = 60
	cfg.Database.UserPlatformQuotaFlusherEnabled = true
	b := &BillingCacheService{cache: cache, cfg: cfg, cacheWriteChan: writes}
	p := &postUsageBillingParams{BalanceAlreadyReserved: true, Platform: PlatformOpenAI, User: &User{ID: 1}, Account: &Account{ID: 713}, Cost: &CostBreakdown{ActualCost: .3}}
	finalizePostUsageBilling(context.Background(), p, &billingDeps{billingCacheService: b, deferredService: &DeferredService{}, userPlatformQuotaRepo: &fakeQuotaRepo{}, cfg: cfg}, &UsageBillingApplyResult{Applied: true})
	require.Empty(t, writes, "durable hold already deducted spendable balance")
	require.Equal(t, []incrCall{{userID: 1, platform: PlatformOpenAI, cost: .3, ttl: time.Minute, markDirty: true}}, cache.calls)
}
