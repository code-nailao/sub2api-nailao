//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGPT62ServiceTierCapabilitiesAndSave(t *testing.T) {
	for _, models := range [][]string{
		{"gpt-6.2-sol"}, {"gpt-6.2-astra"},
		{"gpt-6.1-sol", "gpt-6.2-sol"}, {"gpt-6-astra", "gpt-6.2-astra"},
		{"openai/GPT-6.2_SOL-max", "gpt-6.2-astra-openai-compact"},
	} {
		fast, ultra := 2.5, 8.0
		card := ChannelModelPricing{Models: models, BillingMode: BillingModeToken, FastMultiplier: &fast, UltrafastMultiplier: &ultra}
		require.NoError(t, validateChannelConfig([]ChannelModelPricing{card}, nil), models)
		for _, model := range models {
			require.True(t, supportsGroupFastModel(model), model)
			require.True(t, supportsGroupUltrafastModel(model), model)
			require.Nil(t, validateGroupModelServiceTier(model, "priority"), model)
			require.Nil(t, validateGroupModelServiceTier(model, "ultrafast"), model)
		}
	}
	for _, model := range []string{"gpt-6.2-solitude", "gpt-6.2-astra-preview", "gpt-6.2-luna", "gpt-6.3-sol"} {
		require.False(t, supportsGroupFastModel(model), model)
		require.False(t, supportsGroupUltrafastModel(model), model)
	}
}

func TestGPT62ServiceTierPricesAndInheritance(t *testing.T) {
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, alias := range []string{model, "openai/" + model + "-max", model + "-openai-compact"} {
			require.Equal(t, model, normalizeKnownOpenAICodexModel(alias))
			for _, explicit := range []bool{false, true} {
				billing := newTestBillingService()
				// Isolated catalog fixture; this does not register or change production base prices.
				base := ModelPricing{InputPricePerToken: 3e-6, OutputPricePerToken: 15e-6,
					CacheReadPricePerToken: .3e-6, CacheCreationPricePerToken: 3.75e-6}
				billing.fallbackPrices[model] = &base
				fast, ultra := 2.5, 8.0
				input, output, cached, write := 8e-6, 40e-6, .4e-6, 10e-6
				card := ChannelModelPricing{Models: []string{model}, BillingMode: BillingModeToken,
					InputPrice: &input, OutputPrice: &output, CacheReadPrice: &cached, CacheWritePrice: &write}
				if explicit {
					card.FastMultiplier, card.UltrafastMultiplier = &fast, &ultra
				} else {
					fast, ultra = 2, 6
				}
				group := &Group{ID: 226, Platform: PlatformOpenAI, RateMultiplier: .12, ModelPricing: []ChannelModelPricing{card}}
				tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, CacheCreationTokens: 100}
				for _, tier := range []struct {
					name       string
					multiplier float64
				}{{"default", 1}, {"fast", fast}, {"priority", fast}, {"ultrafast", ultra}} {
					cost, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: alias, Group: group, GroupID: &group.ID,
						Tokens: tokens, RateMultiplier: .12, ServiceTier: tier.name, Resolver: NewModelPricingResolver(nil, billing)})
					require.NoError(t, err, "%s/%s explicit=%t", alias, tier.name, explicit)
					require.InDelta(t, 1000*input*tier.multiplier, cost.InputCost, 1e-10)
					require.InDelta(t, 500*output*tier.multiplier, cost.OutputCost, 1e-10)
					require.InDelta(t, 200*cached*tier.multiplier, cost.CacheReadCost, 1e-10)
					require.InDelta(t, 100*write*tier.multiplier, cost.CacheCreationCost, 1e-10)
					require.InDelta(t, cost.TotalCost*.12, cost.ActualCost, 1e-10)
					require.InDelta(t, tier.multiplier, cost.ServiceTierMultiplier, 1e-10)
				}
				require.Equal(t, 3e-6, base.InputPricePerToken)
				require.Equal(t, 15e-6, base.OutputPricePerToken)
				require.Equal(t, .3e-6, base.CacheReadPricePerToken)
				require.Equal(t, 3.75e-6, base.CacheCreationPricePerToken)
				require.Equal(t, input, *group.ModelPricing[0].InputPrice)
				require.Equal(t, output, *group.ModelPricing[0].OutputPrice)
				require.Equal(t, cached, *group.ModelPricing[0].CacheReadPrice)
				require.Equal(t, write, *group.ModelPricing[0].CacheWritePrice)
			}
		}
	}
}
