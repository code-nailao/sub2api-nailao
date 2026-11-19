package service

import "math"

// UsageBillingSnapshot is write-once evidence for this request, never rebuilt
// from current group settings when reading a historical usage row.
type UsageBillingSnapshot struct {
	DeepSeekTools           *DeepSeekToolUsage `json:"deepseek_tools,omitempty"`
	ChargeKind              string             `json:"charge_kind,omitempty"`
	FixedCharge             float64            `json:"fixed_charge,omitempty"`
	OriginalStatus          int                `json:"original_status,omitempty"`
	RejectionRuleID         string             `json:"rejection_rule_id,omitempty"`
	RejectionRuleVersion    string             `json:"rejection_rule_version,omitempty"`
	Version                 int                `json:"version"`
	RequestedTier           *string            `json:"requested_tier,omitempty"`
	ForwardedTier           string             `json:"forwarded_tier"`
	ObservedTier            string             `json:"observed_tier"`
	BilledTier              string             `json:"billed_tier"`
	FixedUpstreamTier       string             `json:"fixed_upstream_tier,omitempty"`
	TierMultiplier          float64            `json:"tier_multiplier,omitempty"`
	ChargedTierMultiplier   float64            `json:"charged_tier_multiplier,omitempty"`
	GroupMultiplier         float64            `json:"group_multiplier"`
	EffectiveRateMultiplier float64            `json:"effective_rate_multiplier"`
	FreeFastApplied         bool               `json:"free_fast_applied"`
	EffectiveUnitPrices     map[string]float64 `json:"effective_unit_prices"`
}

// Zero means component-specific prices do not have a common scalar multiplier.
func describedServiceTierMultiplier(pricing *ModelPricing, tier string) float64 {
	if !usePriorityServiceTierPricing(tier, pricing) {
		return configuredServiceTierMultiplier(tier, pricing)
	}
	var multiplier float64
	for _, pair := range [][2]float64{
		{pricing.InputPricePerToken, pricing.InputPricePerTokenPriority},
		{pricing.OutputPricePerToken, pricing.OutputPricePerTokenPriority},
		{pricing.CacheReadPricePerToken, pricing.CacheReadPricePerTokenPriority},
		{pricing.CacheCreationPricePerToken, pricing.CacheCreationPricePerTokenPriority},
	} {
		if pair[0] <= 0 {
			continue
		}
		ratio := 1.0
		if pair[1] > 0 {
			ratio = pair[1] / pair[0]
		}
		if multiplier != 0 && math.Abs(multiplier-ratio) > 1e-9 {
			return 0
		}
		multiplier = ratio
	}
	return multiplier
}

func newUsageBillingSnapshot(requested *string, resolution ServiceTierBillingResolution, cost *CostBreakdown, tokens UsageTokens, groupMultiplier, effectiveRate float64, freeFast bool) *UsageBillingSnapshot {
	if cost == nil {
		return nil
	}
	snapshot := &UsageBillingSnapshot{
		Version: 1, ForwardedTier: resolution.Requested, ObservedTier: resolution.Observed,
		BilledTier: resolution.Billing, TierMultiplier: cost.ServiceTierMultiplier,
		ChargedTierMultiplier: cost.ServiceTierMultiplier, GroupMultiplier: groupMultiplier,
		EffectiveRateMultiplier: effectiveRate, FreeFastApplied: freeFast,
		EffectiveUnitPrices: map[string]float64{},
	}
	if requested != nil {
		value := *requested
		snapshot.RequestedTier = &value
	}
	if resolution.FixedUpstreamTier != "" {
		snapshot.Version = 2
		snapshot.FixedUpstreamTier = resolution.FixedUpstreamTier
	}
	if freeFast {
		snapshot.ChargedTierMultiplier = 1
	}
	// Effective average prices are recorded only for non-empty usage buckets.
	// This also preserves time, context and reasoning adjustments exactly.
	for _, item := range []struct {
		key   string
		count int
		cost  float64
	}{
		{"input", tokens.InputTokens - tokens.ImageInputTokens, cost.InputCost},
		{"output", tokens.OutputTokens - tokens.ImageOutputTokens, cost.OutputCost},
		{"cache_read", tokens.CacheReadTokens, cost.CacheReadCost},
		{"cache_write", tokens.CacheCreationTokens, cost.CacheCreationCost},
		{"image_input", tokens.ImageInputTokens, cost.ImageInputCost},
		{"image_output", tokens.ImageOutputTokens, cost.ImageOutputCost},
	} {
		if item.count > 0 {
			snapshot.EffectiveUnitPrices[item.key] = item.cost / float64(item.count)
		}
	}
	return snapshot
}
