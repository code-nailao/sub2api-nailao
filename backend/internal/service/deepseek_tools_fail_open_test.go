//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type deepSeekFailingJournalFixture struct{ deepSeekJournalFixture }

func (j *deepSeekFailingJournalFixture) BeginDeepSeekUsage(context.Context, string, int64, int64, int64, time.Time) error {
	return errors.New("fixture journal offline")
}

type deepSeekFailOpenSetup struct {
	protocol string
	body     map[string]any
	tools    DeepSeekToolsConfig
	// extra replaces the fully configured tool account settings when non-nil.
	extra   map[string]any
	journal UsageBillingRepository
}

var deepSeekReadyToolAccountExtra = map[string]any{"deepseek_tools_enabled": true, "deepseek_search_cost_per_call": .005, "deepseek_fetch_cost_per_call": .005}

func deepSeekExplicitWebSearchTool(protocol string) map[string]any {
	if protocol == "messages" {
		return map[string]any{"type": "web_search_20250305", "name": "web_search"}
	}
	return map[string]any{"type": "web_search"}
}

// runDeepSeekFailOpen calls only the tool hook. Any upstream request fails the
// test: a setup problem must be decided before anything is sent.
func runDeepSeekFailOpen(t *testing.T, setup deepSeekFailOpenSetup) (handled bool, err error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	price, groupID := 6.0, int64(7)
	if setup.tools.FetchPricePer1K == nil {
		setup.tools.FetchPricePer1K = &price
	}
	extra := setup.extra
	if extra == nil {
		extra = deepSeekReadyToolAccountExtra
	}
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: setup.tools}}
	account := &Account{ID: 4, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://provider.example/v1", "api_protocol": "chat_completions"}, Extra: extra}
	journal := setup.journal
	if journal == nil {
		journal = &deepSeekJournalFixture{}
	}
	cfg := deepSeekChatFallbackTestConfig()
	upstream := deepSeekHTTPFixture{fn: func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected upstream request to %s", r.URL.Path)
		return nil, errors.New("unexpected upstream request")
	}}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}, usageBillingRepo: journal, billingService: newTestBillingService(), billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
	body := map[string]any{"model": "deepseek-flash"}
	for k, v := range setup.body {
		body[k] = v
	}
	switch setup.protocol {
	case "responses":
		body["input"] = "hello"
	case "messages":
		body["max_tokens"] = 64
		fallthrough
	default:
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	}
	wire, marshalErr := json.Marshal(body)
	require.NoError(t, marshalErr)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(string(wire)))
	c.Set("api_key", key)
	SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})
	result, handled, err := svc.tryForwardDeepSeekTools(context.Background(), c, account, wire, setup.protocol, "")
	require.Nil(t, result)
	require.Zero(t, recorder.Body.Len(), "nothing may be written before the request is forwarded")
	return handled, err
}

func TestDeepSeekToolsDisabledGroupIsServedWithoutTheFeature(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			// Not even a tool the client names itself turns the request into an error.
			handled, err := runDeepSeekFailOpen(t, deepSeekFailOpenSetup{
				protocol: protocol,
				body:     map[string]any{"tools": []any{deepSeekExplicitWebSearchTool(protocol)}},
				tools:    DeepSeekToolsConfig{Enabled: false, AutoInject: true},
				extra:    map[string]any{},
			})
			require.NoError(t, err)
			require.False(t, handled)
		})
	}
}

func TestDeepSeekAutoInjectedToolsNeverFailTheRequest(t *testing.T) {
	pinned := int64(4)
	enabled := DeepSeekToolsConfig{Enabled: true, AutoInject: true}
	cases := map[string]deepSeekFailOpenSetup{
		"tool_account_not_enabled":    {protocol: "chat", tools: enabled, extra: map[string]any{}},
		"pinned_tool_account_not_set": {protocol: "chat", tools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, AccountID: &pinned}, extra: map[string]any{"deepseek_tools_enabled": false}},
		"request_shape_unsupported":   {protocol: "chat", tools: enabled, body: map[string]any{"n": 2}},
		"previous_response_id":        {protocol: "responses", tools: enabled, body: map[string]any{"previous_response_id": "resp_1"}},
		"background_request":          {protocol: "responses", tools: enabled, body: map[string]any{"background": true}},
		"usage_journal_unavailable":   {protocol: "messages", tools: enabled, journal: &deepSeekFailingJournalFixture{}},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			handled, err := runDeepSeekFailOpen(t, setup)
			require.NoError(t, err)
			require.False(t, handled, "the request must continue on the ordinary path")
		})
	}
}

func TestDeepSeekExplicitToolSetupFailureIsStillReported(t *testing.T) {
	enabled := DeepSeekToolsConfig{Enabled: true}
	cases := map[string]deepSeekFailOpenSetup{
		"tool_account_not_enabled": {protocol: "chat", tools: enabled, extra: map[string]any{}},
		"background_request":       {protocol: "responses", tools: enabled, body: map[string]any{"background": true}},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			if setup.body == nil {
				setup.body = map[string]any{}
			}
			setup.body["tools"] = []any{deepSeekExplicitWebSearchTool(setup.protocol)}
			handled, err := runDeepSeekFailOpen(t, setup)
			require.Error(t, err)
			require.True(t, handled)
		})
	}
}

// A group that turns on automatic tools while its tool account is not ready
// used to fail every request of the group. The request must reach the model
// unchanged and be billed as an ordinary request.
func TestDeepSeekChatSucceedsWhenAutoInjectedToolsAreMisconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	price, groupID, pinned := 6.0, int64(7), int64(31)
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, AccountID: &pinned, FetchPricePer1K: &price}}}
	account := deepSeekForcedToolChoiceAccount()
	account.GroupIDs = []int64{groupID}
	account.Extra = map[string]any{"deepseek_tools_enabled": false}
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)
	c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
	c.Set("api_key", key)
	upstream := newOKChatCompletionsUpstream("rid_ds_fail_open", deepSeekChatFallbackOKBody)
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.DeepSeekTools)
	require.Contains(t, upstream.lastReq.URL.String(), "/chat/completions")
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools").Exists(), "no gateway tool may be added: %s", upstream.lastBody)
	require.Equal(t, "hi", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
}
