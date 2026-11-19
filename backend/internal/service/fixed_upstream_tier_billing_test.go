//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFixedUpstreamTierRetainsSelectedCredentialPrice(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, tier := range []string{OpenAIFastTierPriority, OpenAIFastTierUltrafast} {
		for _, observed := range []string{"", "default", "standard", "priority", "ultrafast", "flex"} {
			t.Run(tier+"/"+observed, func(t *testing.T) {
				clientTier := "flex"
				result := &OpenAIForwardResult{FixedUpstreamRequestTier: tier, ServiceTier: &clientTier, UpstreamResponseServiceTier: observed}
				resolution := ApplyOpenAIServiceTierBillingResolution(account, result)
				require.Equal(t, tier, *result.ServiceTier)
				require.Equal(t, tier, resolution.Billing)
				require.Equal(t, tier, resolution.FixedUpstreamTier)
				require.Empty(t, resolution.Requested, "no service_tier was sent on the wire")
				require.Equal(t, observed, resolution.Observed)
				require.False(t, resolution.Downgraded)
				require.Equal(t, resolution, ApplyOpenAIServiceTierBillingResolution(account, result), "resolution is idempotent")

				generic := &ForwardResult{FixedUpstreamRequestTier: tier, UpstreamResponseServiceTier: observed}
				require.Equal(t, resolution, ApplyForwardServiceTierBillingResolution(generic))
				require.Equal(t, tier, *generic.ServiceTier)
			})
		}
	}
}

func TestFixedUpstreamTierDoesNotChangeLegacyResponseContract(t *testing.T) {
	for _, marker := range []string{"", "invalid", "fast"} {
		requested := "ultrafast"
		result := &OpenAIForwardResult{FixedUpstreamRequestTier: marker, ServiceTier: &requested, UpstreamResponseServiceTier: "default"}
		resolution := ApplyOpenAIServiceTierBillingResolution(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, result)
		require.Equal(t, "default", resolution.Billing)
		require.True(t, resolution.Downgraded)
		require.Empty(t, resolution.FixedUpstreamTier)
	}
}

func TestFixedUpstreamTierPricingAndSnapshotApplyMultiplierOnce(t *testing.T) {
	for _, tt := range []struct {
		name, tier string
		custom     bool
		wantRatio  float64
	}{
		{"fast default", "priority", false, 2},
		{"ultra default", "ultrafast", false, 6},
		{"fast custom", "priority", true, 3},
		{"ultra custom", "ultrafast", true, 7.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			group := &Group{ID: 2, Platform: PlatformOpenAI, RateMultiplier: .15}
			if tt.custom {
				fast, ultra := 3.0, 7.5
				group.ModelPricing = []ChannelModelPricing{{Models: []string{"gpt-6.1-sol"}, BillingMode: BillingModeToken, FastMultiplier: &fast, UltrafastMultiplier: &ultra}}
			}
			billing := newTestBillingService()
			result := &OpenAIForwardResult{FixedUpstreamRequestTier: tt.tier, UpstreamResponseServiceTier: "default"}
			resolution := ApplyOpenAIServiceTierBillingResolution(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, result)
			tokens := UsageTokens{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 500}
			cost, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: "gpt-6.1-sol", Group: group, GroupID: &group.ID, Tokens: tokens, RateMultiplier: .15, ServiceTier: *result.ServiceTier, Resolver: NewModelPricingResolver(nil, billing)})
			require.NoError(t, err)
			require.InDelta(t, (1000*2e-6+200*10e-6+500*.1e-6)*tt.wantRatio, cost.TotalCost, 1e-12)
			require.InDelta(t, cost.TotalCost*.15, cost.ActualCost, 1e-12)
			require.Equal(t, tt.wantRatio, cost.ServiceTierMultiplier)
			clientRequested := "ultrafast"
			snapshot := newUsageBillingSnapshot(&clientRequested, resolution, cost, tokens, .15, .15, false)
			require.Equal(t, 2, snapshot.Version)
			require.Equal(t, tt.tier, snapshot.FixedUpstreamTier)
			require.Equal(t, tt.tier, snapshot.BilledTier)
			require.Empty(t, snapshot.ForwardedTier)
			require.Equal(t, "default", snapshot.ObservedTier)
			require.Equal(t, tt.wantRatio, snapshot.ChargedTierMultiplier)
			clientRequested = "flex"
			cost.InputCost = 100
			require.Equal(t, "ultrafast", *snapshot.RequestedTier)
			require.InDelta(t, 2e-6*tt.wantRatio, snapshot.EffectiveUnitPrices["input"], 1e-12)
		})
	}
}

func TestPrivateUsageBillingBuilderPreservesRawAmountsForRepositoryValidation(t *testing.T) {
	p := &postUsageBillingParams{
		Cost: &CostBreakdown{TotalCost: 1, ActualCost: 1}, APIKey: &APIKey{ID: 1}, User: &User{ID: 2},
		Account:               &Account{ID: 3, Type: AccountTypeAPIKey, Extra: map[string]any{"quota_limit": 10.0}},
		AccountRateMultiplier: -1e-12, UpstreamRejectionReservationID: "hold-raw-validation",
	}
	cmd := buildUsageBillingCommand("raw-command", nil, p)
	require.NotNil(t, cmd)
	require.Equal(t, -1e-12, cmd.AccountQuotaCost, "repository must see the invalid original amount")
	require.Empty(t, cmd.RequestFingerprint, "private repository owns validation then normalization")
	p.UpstreamRejectionReservationID = ""
	ordinary := buildUsageBillingCommand("ordinary-command", nil, p)
	require.Zero(t, ordinary.AccountQuotaCost, "ordinary preexisting normalization behavior remains unchanged")
	require.NotEmpty(t, ordinary.RequestFingerprint)
}

func TestFixedUpstreamTierRecordUsageRestoresContextAndWritesEvidence(t *testing.T) {
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		t.Run(tier, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			group := &Group{ID: 2, Platform: PlatformOpenAI, RateMultiplier: .15, RequestSpeedSelectionEnabled: true}
			ctx := WithRequestSpeed(context.Background(), group.ID, tier)
			clientTier := "ultrafast"
			result := &OpenAIForwardResult{
				RequestID: "fixed-usage-" + tier, Model: "gpt-6.1-sol",
				RequestedServiceTier: &clientTier, UpstreamResponseServiceTier: "default",
				Usage: OpenAIUsage{InputTokens: 1000, OutputTokens: 200},
			}
			err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: result,
				APIKey: &APIKey{ID: 10, GroupID: &group.ID, Group: group}, User: &User{ID: 20},
				Account: &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			})
			require.NoError(t, err)
			row := usageRepo.lastLog
			require.NotNil(t, row)
			wantTier, wantRatio := OpenAIFastTierPriority, 2.0
			if tier == RequestSpeedUltrafast {
				wantTier, wantRatio = OpenAIFastTierUltrafast, 6
			}
			require.Equal(t, wantTier, *row.ServiceTier)
			require.InDelta(t, (1000*2e-6+200*10e-6)*wantRatio, row.TotalCost, 1e-12)
			require.InDelta(t, row.TotalCost*.15, row.ActualCost, 1e-12)
			require.NotNil(t, row.BillingSnapshot)
			require.Equal(t, 2, row.BillingSnapshot.Version)
			require.Equal(t, wantTier, row.BillingSnapshot.FixedUpstreamTier)
			require.Equal(t, "default", row.BillingSnapshot.ObservedTier)
			require.Empty(t, row.BillingSnapshot.ForwardedTier)
			require.Equal(t, "ultrafast", *row.BillingSnapshot.RequestedTier)
		})
	}
}
