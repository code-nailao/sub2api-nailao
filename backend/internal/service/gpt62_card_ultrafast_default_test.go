//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Pins the Sources deviation in keepSourcesGPT62CardUltrafastDefault: GPT-6.2
// price cards without an Ultra Fast value keep 6x on both sides of upstream's
// 2027-03-01 08:00 UTC switch.
func TestGPT62CardUltrafastDefaultStaysSixAtEveryDate(t *testing.T) {
	before := time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC)
	after := before.Add(time.Second)
	input, output := 8e-6, 40e-6
	tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500}
	cost := func(t *testing.T, model string, card ChannelModelPricing, at time.Time) *CostBreakdown {
		t.Helper()
		billing := newTestBillingService()
		group := &Group{ID: 227, Platform: PlatformOpenAI, RateMultiplier: 1, ModelPricing: []ChannelModelPricing{card}}
		breakdown, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: model, Group: group, GroupID: &group.ID,
			Tokens: tokens, RateMultiplier: 1, ServiceTier: "ultrafast", PricingAt: at, Resolver: NewModelPricingResolver(nil, billing)})
		require.NoError(t, err)
		return breakdown
	}
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra", "openai/gpt-6.2-sol-max"} {
		for _, at := range []time.Time{before, after} {
			card := ChannelModelPricing{Models: []string{model}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}
			got := cost(t, model, card, at)
			require.InDelta(t, 6, got.ServiceTierMultiplier, 1e-12, "%s at %s", model, at)
			require.InDelta(t, (1000*input+500*output)*6, got.TotalCost, 1e-12, "%s at %s", model, at)

			explicit := 3.0
			card.UltrafastMultiplier = &explicit
			got = cost(t, model, card, at)
			require.InDelta(t, 3, got.ServiceTierMultiplier, 1e-12, "%s at %s", model, at)
			require.InDelta(t, (1000*input+500*output)*3, got.TotalCost, 1e-12, "%s at %s", model, at)
		}
	}

	// Without a card the request follows upstream's catalogue schedule.
	billing := newTestBillingService()
	for at, want := range map[time.Time]float64{before: 3, after: 6} {
		got, err := billing.CalculateCostUnified(CostInput{Model: "gpt-6.2-sol", Tokens: tokens, RateMultiplier: 1, ServiceTier: "ultrafast", PricingAt: at, Resolver: NewModelPricingResolver(nil, billing)})
		require.NoError(t, err)
		require.InDelta(t, want, got.ServiceTierMultiplier, 1e-12, at)
	}
}

func TestGPT62CardUltrafastDefaultLeavesOtherPricingUntouched(t *testing.T) {
	pricing := &ModelPricing{UltrafastMultiplier: 4}
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-5.5", "claude-opus-5-5", "gpt-6.2-luna"} {
		for _, source := range []string{PricingSourceChannel, PricingSourceGroup} {
			require.Same(t, pricing, keepSourcesGPT62CardUltrafastDefault(model, pricing, source), model)
		}
	}
	for _, source := range []string{PricingSourceLiteLLM, PricingSourceFallback, ""} {
		require.Same(t, pricing, keepSourcesGPT62CardUltrafastDefault("gpt-6.2-sol", pricing, source), source)
	}
	explicit := 2.0
	withValue := &ModelPricing{UltrafastMultiplier: 3, UltrafastMultiplierOverride: &explicit}
	require.Same(t, withValue, keepSourcesGPT62CardUltrafastDefault("gpt-6.2-sol", withValue, PricingSourceChannel))
	require.Nil(t, keepSourcesGPT62CardUltrafastDefault("gpt-6.2-sol", nil, PricingSourceChannel))

	kept := keepSourcesGPT62CardUltrafastDefault("gpt-6.2-astra", pricing, PricingSourceChannel)
	require.NotSame(t, pricing, kept)
	require.Equal(t, 6.0, kept.UltrafastMultiplier)
	require.Equal(t, 4.0, pricing.UltrafastMultiplier, "the shared price must not be mutated")
}
