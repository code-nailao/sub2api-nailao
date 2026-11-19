//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGroupUltrafastActualHTTPForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra"} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 226,
					Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, OpenAIServiceTierPolicy: GroupTierForceUltrafast})
				body := []byte(fmt.Sprintf(`{"model":%q,"input":"hello","stream":%t}`, model, stream))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
				response := fmt.Sprintf(`{"id":"resp_tier","object":"response","status":"completed","model":%q,"service_tier":"priority","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, model)
				contentType := "application/json"
				if stream {
					response = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
					contentType = "text/event-stream"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}}
				svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
				svc.cfg = &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}
				svc.httpUpstream = upstream
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"api_key": "test-only"}, Extra: map[string]any{"openai_responses_supported": true}}
				result, err := svc.Forward(ctx, c, account, body)
				require.NoError(t, err)
				require.Equal(t, "ultrafast", gjson.GetBytes(upstream.lastBody, "service_tier").String())
				require.Equal(t, "ultrafast", *result.ServiceTier)
				require.Equal(t, "priority", result.UpstreamResponseServiceTier)
				resolution := ApplyOpenAIServiceTierBillingResolution(account, result)
				require.True(t, resolution.Downgraded)
				require.Equal(t, "priority", resolution.Billing)
			})
		}
	}
}

func TestGroupServiceTierPricingMatrix(t *testing.T) {
	for _, model := range []struct {
		canonical                    string
		aliases                      []string
		input, output, cached, write float64
	}{
		{"gpt-6-astra", []string{"gpt-6-astra", "gpt-6", "openai/gpt-6-astra-max"}, 10e-6, 50e-6, 1e-6, 12.5e-6},
		{"gpt-6.1-sol", []string{"gpt-6.1-sol", "openai/gpt-6.1-sol-max"}, 2e-6, 10e-6, .1e-6, 2.5e-6},
		{"gpt-6-luna", []string{"gpt-6-luna", "openai/gpt-6-luna-max"}, .1e-6, .5e-6, .01e-6, .125e-6},
	} {
		for _, preset := range []struct {
			name        string
			fast, ultra float64
			explicit    bool
		}{
			{"default", 2, 6, false}, {"api", 2, 6, true}, {"subscription", 2.5, 8, true}, {"custom", 3, 7.5, true},
		} {
			for _, alias := range model.aliases {
				for _, totalInput := range []int{271999, 272000, 272001} {
					for _, tier := range []string{"default", "fast", "priority", "ultrafast"} {
						if tier == "ultrafast" && !supportsGroupUltrafastModel(model.canonical) {
							continue
						}
						t.Run(fmt.Sprintf("%s/%s/%d/%s", alias, preset.name, totalInput, tier), func(t *testing.T) {
							billing := newTestBillingService()
							group := &Group{ID: 226, Platform: PlatformOpenAI, RateMultiplier: .12, LongContextPricingEnabled: true}
							if preset.explicit {
								card := ChannelModelPricing{Models: []string{model.canonical}, BillingMode: BillingModeToken, FastMultiplier: &preset.fast}
								if supportsGroupUltrafastModel(model.canonical) {
									card.UltrafastMultiplier = &preset.ultra
								}
								group.ModelPricing = []ChannelModelPricing{card}
							}
							tokens := UsageTokens{InputTokens: totalInput - 3000, CacheReadTokens: 2000, CacheCreationTokens: 1000, OutputTokens: 500}
							cost, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: alias, Group: group, GroupID: &group.ID, Tokens: tokens, RateMultiplier: .12, ServiceTier: tier, Resolver: NewModelPricingResolver(nil, billing)})
							require.NoError(t, err)
							multiplier := 1.0
							if tier == "fast" || tier == "priority" {
								multiplier = preset.fast
							}
							if tier == "ultrafast" {
								multiplier = preset.ultra
							}
							im, om := 1.0, 1.0
							if totalInput > 272000 {
								im, om = 2, 1.5
							}
							require.InDelta(t, float64(tokens.InputTokens)*model.input*im*multiplier, cost.InputCost, 1e-9)
							require.InDelta(t, 2000*model.cached*im*multiplier, cost.CacheReadCost, 1e-9)
							require.InDelta(t, 1000*model.write*im*multiplier, cost.CacheCreationCost, 1e-9)
							require.InDelta(t, 500*model.output*om*multiplier, cost.OutputCost, 1e-9)
							require.InDelta(t, cost.TotalCost*.12, cost.ActualCost, 1e-9)
							require.InDelta(t, multiplier, cost.ServiceTierMultiplier, 1e-9)
							base, err := billing.GetModelPricing(model.canonical)
							require.NoError(t, err)
							require.Equal(t, model.input, base.InputPricePerToken, "preset must not mutate standard model prices")
							require.Equal(t, model.output, base.OutputPricePerToken)
						})
					}
				}
			}
		}
	}
}

func TestGroupUltrafastSelectionHTTPAndWS(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, model := range []string{"gpt-6-astra", "gpt-6", "gpt-6.1-sol", "openai/gpt-6.1-sol-max", "gpt-6.2-sol", "openai/gpt-6.2-astra-max"} {
		for _, tier := range []string{"", "default", "fast", "priority", "ultrafast", "flex"} {
			svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
			group := &Group{ID: 226, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, OpenAIServiceTierPolicy: GroupTierForceUltrafast}
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"service_tier":%q}`, model, tier))
			httpBody, err := svc.applyOpenAIFastPolicyToBody(ctx, account, model, body)
			require.NoError(t, err)
			wsBody, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, body)
			require.NoError(t, err)
			require.Nil(t, blocked)
			require.JSONEq(t, string(httpBody), string(wsBody))
			require.Equal(t, "ultrafast", gjson.GetBytes(httpBody, "service_tier").String())
			group.FreeOpenAIFast = true
			require.False(t, groupBillsOpenAIFastAtStandard(&APIKey{Group: group}, account, "ultrafast"))
		}
	}
}

func TestGroupUltrafastCannotSilentlyDowngrade(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 226, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, OpenAIServiceTierPolicy: GroupTierForceUltrafast})
	for _, action := range []string{BetaPolicyActionBlock, BetaPolicyActionFilter, OpenAIFastPolicyActionForcePriority} {
		svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true, Rules: []OpenAIFastPolicyRule{{ServiceTier: "all", Scope: "all", Action: action}}})
		_, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6-astra", []byte(`{"model":"gpt-6-astra"}`))
		require.Error(t, err)
	}
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
	_, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6-luna", []byte(`{"model":"gpt-6-luna"}`))
	require.Error(t, err)
	svc = newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	_, err = svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6-astra", []byte(`{"model":"gpt-6-astra"}`))
	require.Error(t, err, "disabled feature cannot serve the Ultra group as Standard")
}

func TestGroupPassthroughOverridesLegacyForce(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 226, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ForceOpenAIFast: true, OpenAIServiceTierPolicy: GroupTierPassthrough})
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
	updated, err := svc.applyOpenAIFastPolicyToBody(ctx, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-6-astra", []byte(`{"model":"gpt-6-astra","service_tier":"ultrafast"}`))
	require.NoError(t, err)
	require.Equal(t, "ultrafast", gjson.GetBytes(updated, "service_tier").String())
}

func TestUsageBillingSnapshotCopiesRatesAndPreservesFreeFast(t *testing.T) {
	requested := "ultrafast"
	resolution := ResolveBillingServiceTier("ultrafast", "priority")
	cost := &CostBreakdown{InputCost: .004, OutputCost: .02, ServiceTierMultiplier: 2}
	snapshot := newUsageBillingSnapshot(&requested, resolution, cost, UsageTokens{InputTokens: 1000, OutputTokens: 1000}, .12, .12, true)
	requested = "default"
	cost.InputCost = 999
	require.Equal(t, "ultrafast", *snapshot.RequestedTier)
	require.Equal(t, "ultrafast", snapshot.ForwardedTier)
	require.Equal(t, "priority", snapshot.BilledTier)
	require.Equal(t, 2.0, snapshot.TierMultiplier)
	require.Equal(t, 1.0, snapshot.ChargedTierMultiplier)
	require.Equal(t, .12, snapshot.GroupMultiplier)
	require.Equal(t, .004/1000, snapshot.EffectiveUnitPrices["input"])
}

func TestGroupServiceTierModelScope(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, model := range []string{"gpt-6-astra", "gpt-6", "openai/gpt-6.1-sol-max", "gpt-6-luna", "openai/gpt-6-luna-high", "gpt-6-sol", "gpt-5.6-sol", "gpt-image-2", "gpt-6-luna-preview", "gpt-6.1-solitude"} {
		for _, policy := range []string{GroupTierPassthrough, GroupTierForceFast, GroupTierForceUltrafast} {
			for _, requested := range []string{"", "default", "fast", "ultrafast"} {
				t.Run(model+"/"+policy+"/"+requested, func(t *testing.T) {
					group := &Group{ID: 226, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, OpenAIServiceTierPolicy: policy}
					ctx := context.WithValue(context.Background(), ctxkey.Group, group)
					svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
					effective := requested
					if policy == GroupTierForceFast {
						effective = "fast"
					}
					if policy == GroupTierForceUltrafast {
						effective = "ultrafast"
					}
					wantBlocked := validateGroupModelServiceTier(model, effective) != nil
					body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"service_tier":%q}`, model, requested))
					_, err := svc.applyOpenAIFastPolicyToBody(ctx, account, model, body)
					require.Equal(t, wantBlocked, err != nil)
					_, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, body)
					require.NoError(t, err)
					require.Equal(t, wantBlocked, blocked != nil)
				})
			}
		}
	}
	// Old groups keep their existing settings; rollout does not rewrite them.
	group := &Group{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ForceOpenAIFast: true}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
	tier, _, blocked := svc.resolveOpenAIRequestServiceTier(ctx, account, "gpt-5.4", "default")
	require.Nil(t, blocked)
	require.Equal(t, "priority", tier)
}

func TestGroupTierCapabilitiesAndPriceCardScope(t *testing.T) {
	for _, tc := range []struct {
		model       string
		fast, ultra bool
	}{
		{"gpt-6-astra", true, true}, {"gpt-6", true, true}, {"openai/gpt-6.1-sol-max", true, true},
		{"gpt-6-luna", true, false}, {"openai/gpt-6-luna-high", true, false},
		{"gpt-6-sol", false, false}, {"gpt-5.6-sol", false, false}, {"gpt-image-2", false, false},
		{"gpt-6-luna-preview", false, false}, {"gpt-6.1-solitude", false, false},
	} {
		require.Equal(t, tc.fast, supportsGroupFastModel(tc.model), tc.model)
		require.Equal(t, tc.ultra, supportsGroupUltrafastModel(tc.model), tc.model)
		ultra := 6.0
		err := checkPricesNotNegative(ChannelModelPricing{Models: []string{tc.model}, UltrafastMultiplier: &ultra})
		require.Equal(t, !tc.ultra, err != nil, tc.model)
	}
}

func TestUsageBillingSnapshotSupportedTextScope(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna", "gpt-6-sol", "gpt-5.4"} {
		t.Run(model, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{RequestID: "snapshot-scope-" + model, Model: model, Usage: OpenAIUsage{InputTokens: 20, OutputTokens: 10}},
				APIKey: &APIKey{ID: 10}, User: &User{ID: 20}, Account: &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			})
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.Equal(t, supportsGroupFastModel(model), usageRepo.lastLog.BillingSnapshot != nil)
		})
	}
}
