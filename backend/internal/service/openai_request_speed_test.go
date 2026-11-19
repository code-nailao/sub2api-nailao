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

func fixedRequestSpeedFixture(tier string) (context.Context, *Account) {
	group := &Group{ID: 71, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true,
		RequestSpeedSelectionEnabled: true, DefaultRequestSpeedTier: RequestSpeedUltrafast,
		ForceOpenAIUltrafast: true, OpenAIServiceTierPolicy: GroupTierForceUltrafast}
	ctx := WithRequestSpeed(context.WithValue(context.Background(), ctxkey.Group, group), group.ID, tier)
	account := &Account{ID: 72, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		GroupIDs: []int64{71}, RequestSpeedTiersByGroup: map[string][]string{"71": {tier}}, RateMultiplier: f64p(1),
		Credentials: map[string]any{"api_key": "synthetic-" + tier}, Extra: map[string]any{"openai_responses_supported": true}}
	return ctx, account
}

func TestFixedRequestSpeedKeyResolutionAndGroupIsolation(t *testing.T) {
	gid := int64(71)
	fast := RequestSpeedFast
	key := &APIKey{GroupID: &gid, Group: &Group{ID: gid, Platform: PlatformOpenAI, RequestSpeedSelectionEnabled: true, DefaultRequestSpeedTier: fast}}
	tier, err := ResolveAPIKeyRequestSpeed(key)
	require.NoError(t, err)
	require.Equal(t, RequestSpeedUltrafast, tier, "old nil Key preserves Ultra despite a new default")
	key.RequestSpeedTier = &fast
	tier, err = ResolveAPIKeyRequestSpeed(key)
	require.NoError(t, err)
	require.Equal(t, fast, tier)
	key.Group.RequestSpeedSelectionEnabled = false
	tier, err = ResolveAPIKeyRequestSpeed(key)
	require.NoError(t, err)
	require.Empty(t, tier)
	key.Group.RequestSpeedSelectionEnabled = true
	bad := "client-injected"
	key.RequestSpeedTier = &bad
	_, err = ResolveAPIKeyRequestSpeed(key)
	require.Error(t, err)
	ctx, account := fixedRequestSpeedFixture(fast)
	require.True(t, AccountSupportsRequestSpeed(ctx, account, &gid))
	account.RequestSpeedTiersByGroup["71"] = []string{RequestSpeedFast, RequestSpeedUltrafast}
	require.False(t, AccountSupportsRequestSpeed(ctx, account, &gid), "one fixed supplier credential cannot advertise two tiers")
	account.RequestSpeedTiersByGroup["71"] = []string{RequestSpeedFast}
	other := int64(99)
	require.False(t, AccountSupportsRequestSpeed(ctx, account, &other))
	account.RequestSpeedTiersByGroup = map[string][]string{}
	account.AccountGroups = []AccountGroup{{GroupID: gid, RequestSpeedTiers: []string{fast}}}
	require.False(t, AccountSupportsRequestSpeed(ctx, account, &gid), "cleared native projection must not revive stale association")
	require.True(t, AccountSupportsRequestSpeed(context.Background(), account, &other), "ordinary group eligibility remains native")
}

func TestFixedRequestSpeedPolicyAndModelGuards(t *testing.T) {
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		t.Run(tier, func(t *testing.T) {
			ctx, account := fixedRequestSpeedFixture(tier)
			svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
			body := []byte(`{"model":"gpt-6.1-sol","service_tier":"client-any-value","input":"x"}`)
			out, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", body)
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(out, "service_tier").Exists())
			require.Equal(t, "x", gjson.GetBytes(out, "input").String())
			_, err = svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-5.4", body)
			require.Error(t, err)
			canonical := RequestSpeedServiceTierFromContext(ctx)
			for _, action := range []string{BetaPolicyActionFilter, BetaPolicyActionBlock, OpenAIFastPolicyActionForcePriority} {
				settings := &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: canonical, Action: action, Scope: BetaPolicyScopeAll}}}
				policySvc := newOpenAIGatewayServiceWithSettings(t, settings)
				_, err = policySvc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", body)
				if action == OpenAIFastPolicyActionForcePriority && tier == RequestSpeedFast {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "global rule must not silently rewrite selected supplier tier")
				}
			}
		})
	}
	ctx, account := fixedRequestSpeedFixture(RequestSpeedFast)
	service := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	_, err := service.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6-luna", []byte(`{"model":"gpt-6-luna"}`))
	require.NoError(t, err)
	ctx, _ = fixedRequestSpeedFixture(RequestSpeedUltrafast)
	account.RequestSpeedTiersByGroup["71"] = []string{RequestSpeedUltrafast}
	_, err = service.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6-luna", []byte(`{"model":"gpt-6-luna"}`))
	require.Error(t, err)
}

func TestFixedRequestSpeedHTTPAdaptersStripClientTierAndStampBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		for _, path := range []string{"responses", "passthrough", "chat-responses", "chat-raw", "messages-responses", "messages-raw"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tier, path, stream), func(t *testing.T) {
					ctx, account := fixedRequestSpeedFixture(tier)
					body := fmt.Sprintf(`{"model":"gpt-6.1-sol","input":"hello","instructions":"answer","stream":%t,"service_tier":"flex"}`, stream)
					url := "/v1/responses"
					if strings.HasPrefix(path, "chat") {
						url = "/v1/chat/completions"
						body = fmt.Sprintf(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"stream":%t,"service_tier":"ultrafast"}`, stream)
					}
					if strings.HasPrefix(path, "messages") {
						url = "/v1/messages"
						body = fmt.Sprintf(`{"model":"gpt-6.1-sol","max_tokens":20,"messages":[{"role":"user","content":"hello"}],"stream":%t,"service_tier":"priority"}`, stream)
					}
					response := `{"id":"resp_fixed","object":"response","status":"completed","model":"gpt-6.1-sol","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":10}}}`
					contentType := "application/json"
					if stream || path == "chat-responses" || path == "messages-responses" {
						response = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
						contentType = "text/event-stream"
					}
					if strings.HasSuffix(path, "raw") {
						account.Extra["openai_responses_supported"] = false
						response = `{"id":"chatcmpl_fixed","object":"chat.completion","model":"gpt-6.1-sol","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":10}}}`
						if stream {
							response = "data: {\"id\":\"chatcmpl_fixed\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-6.1-sol\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n"
						}
					}
					if path == "passthrough" {
						account.Extra["openai_passthrough"] = true
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}}
					svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
					svc.cfg = &config.Config{}
					svc.httpUpstream = upstream
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body))).WithContext(ctx)
					var result *OpenAIForwardResult
					var err error
					switch {
					case strings.HasPrefix(path, "chat"):
						result, err = svc.ForwardAsChatCompletions(ctx, c, account, []byte(body), "", "")
					case strings.HasPrefix(path, "messages"):
						result, err = svc.ForwardAsAnthropic(ctx, c, account, []byte(body), "", "")
					default:
						result, err = svc.Forward(ctx, c, account, []byte(body))
					}
					require.NoError(t, err, "body=%s", rec.Body.String())
					require.NotNil(t, result)
					require.NotNil(t, upstream.lastReq)
					require.Equal(t, "Bearer synthetic-"+tier, upstream.lastReq.Header.Get("Authorization"))
					require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists(), "fixed supplier must not receive client or group tier override: %s", upstream.lastBody)
					canonical := RequestSpeedServiceTierFromContext(ctx)
					require.Equal(t, canonical, result.FixedUpstreamRequestTier)
					require.Equal(t, canonical, *result.ServiceTier)
					settled := ApplyOpenAIServiceTierBillingResolution(account, result)
					require.False(t, settled.Downgraded)
					require.Equal(t, canonical, settled.Billing)
				})
			}
		}
	}
}

func TestFixedRequestSpeedPostQueueRechecksMapping(t *testing.T) {
	ctx, old := fixedRequestSpeedFixture(RequestSpeedFast)
	latest := *old
	latest.RequestSpeedTiersByGroup = map[string][]string{"71": {RequestSpeedUltrafast}}
	repo := &requestSpeedRecheckRepo{account: &latest}
	svc := &OpenAIGatewayService{accountRepo: repo}
	_, vetoed, reason := svc.ProfitControlVetoLatest(ctx, old)
	require.True(t, vetoed)
	require.Equal(t, "request_speed_mismatch", reason)
	require.Equal(t, 1, repo.reads)
	// The same account becoming unavailable must not bypass the queue admission.
	latest.RequestSpeedTiersByGroup = map[string][]string{"71": {RequestSpeedFast}}
	latest.Schedulable = false
	_, vetoed, _ = svc.ProfitControlVetoLatest(ctx, old)
	require.True(t, vetoed)
	got, vetoed, _ := svc.ProfitControlVetoLatest(context.Background(), old)
	require.False(t, vetoed)
	require.Same(t, old, got)
}

func TestFixedRequestSpeedRetainsExplicitGroupPolicyPrecedence(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{GroupTierPoliciesEnabled: true})
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, tc := range []struct{ policy, expected string }{{GroupTierPassthrough, "flex"}, {GroupTierForceFast, "priority"}, {GroupTierForceUltrafast, "ultrafast"}, {"", "ultrafast"}} {
		group := &Group{ID: 71, Platform: PlatformOpenAI, Hydrated: true, Status: StatusActive, OpenAIServiceTierPolicy: tc.policy, ForceOpenAIUltrafast: true, ForceOpenAIFast: true}
		ctx := context.WithValue(context.Background(), ctxkey.Group, group)
		out, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol","service_tier":"flex"}`))
		require.NoError(t, err)
		require.Equal(t, tc.expected, gjson.GetBytes(out, "service_tier").String())
	}
}

func TestFixedRequestSpeedFreshGroupAndKeyInvalidation(t *testing.T) {
	ctx, account := fixedRequestSpeedFixture(RequestSpeedUltrafast)
	latest := *account
	latest.AccountGroups = []AccountGroup{{GroupID: 71, RequestSpeedTiers: []string{RequestSpeedUltrafast}, Group: &Group{ID: 71, Platform: PlatformOpenAI, Status: StatusActive, RequestSpeedSelectionEnabled: false}}}
	repo := &requestSpeedRecheckRepo{account: &latest}
	svc := &OpenAIGatewayService{accountRepo: repo}
	require.NotNil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"), "closing a group must invalidate an existing WS turn even when account mapping remains")
	_, blocked, _ := svc.ProfitControlVetoLatest(ctx, account)
	require.True(t, blocked)
	latest.AccountGroups[0].Group.RequestSpeedSelectionEnabled = true
	require.Nil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"))
	changed := false
	ctx = WithRequestSpeedValidator(ctx, func(context.Context) error {
		if changed {
			return ErrRequestSpeedInvalid
		}
		return nil
	})
	require.Nil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"))
	changed = true
	require.NotNil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"), "new WS turn must stop after the authenticated Key changes")
	_, blocked, reason := svc.ProfitControlVetoLatest(ctx, account)
	require.True(t, blocked)
	require.Equal(t, "request_speed_configuration_changed", reason)
}
