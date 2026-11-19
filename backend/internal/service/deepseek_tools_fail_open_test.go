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
	// noSearchPrice leaves the group's Search price unset.
	noSearchPrice bool
}

// deepSeekFailOpenRun is what the tool hook decided: the body it sent and the
// body it hands back for the ordinary path.
type deepSeekFailOpenRun struct {
	sent, forward []byte
	handled       bool
	err           error
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
func runDeepSeekFailOpen(t *testing.T, setup deepSeekFailOpenSetup) deepSeekFailOpenRun {
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
	if setup.noSearchPrice {
		key.Group.SearchPricePer1k = nil
	}
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
	result, forward, handled, err := svc.tryForwardDeepSeekTools(context.Background(), c, account, wire, setup.protocol, "")
	require.Nil(t, result)
	require.Zero(t, recorder.Body.Len(), "nothing may be written before the request is forwarded")
	return deepSeekFailOpenRun{sent: wire, forward: forward, handled: handled, err: err}
}

func TestDeepSeekToolsDisabledGroupIsServedWithoutTheFeature(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			// Not even a tool the client names itself turns the request into an
			// error, and web_search* reaches the ordinary path unchanged.
			run := runDeepSeekFailOpen(t, deepSeekFailOpenSetup{
				protocol: protocol,
				body:     map[string]any{"tools": []any{deepSeekExplicitWebSearchTool(protocol)}},
				tools:    DeepSeekToolsConfig{Enabled: false, AutoInject: true},
				extra:    map[string]any{},
			})
			require.NoError(t, run.err)
			require.False(t, run.handled)
			require.Equal(t, string(run.sent), string(run.forward))
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
			run := runDeepSeekFailOpen(t, setup)
			require.NoError(t, run.err)
			require.False(t, run.handled, "the request must continue on the ordinary path")
			require.Equal(t, string(run.sent), string(run.forward), "tools the gateway would add are simply not added")
		})
	}
}

// Web tools the client declared itself are optional too: when they cannot be
// set up, both tool families are removed, the model is told live information
// could not be checked, and the request is answered as an ordinary one.
func TestDeepSeekDeclaredToolsAreRemovedWhenSetupFails(t *testing.T) {
	enabled := DeepSeekToolsConfig{Enabled: true}
	cases := map[string]deepSeekFailOpenSetup{
		"tool_account_not_enabled":  {protocol: "chat", tools: enabled, extra: map[string]any{}},
		"prices_not_configured":     {protocol: "chat", tools: enabled, noSearchPrice: true},
		"background_request":        {protocol: "responses", tools: enabled, body: map[string]any{"background": true}},
		"previous_response_id":      {protocol: "responses", tools: enabled, body: map[string]any{"previous_response_id": "resp_1"}},
		"request_shape_unsupported": {protocol: "chat", tools: enabled, body: map[string]any{"n": 2}},
		"usage_journal_unavailable": {protocol: "messages", tools: enabled, journal: &deepSeekFailingJournalFixture{}},
	}
	for name, setup := range cases {
		for _, family := range []string{"web_search", "deepseek_web_search"} {
			t.Run(name+"/"+family, func(t *testing.T) {
				if setup.body == nil {
					setup.body = map[string]any{}
				}
				search := deepSeekExplicitWebSearchTool(setup.protocol)
				if family == "deepseek_web_search" {
					search = map[string]any{"type": "deepseek_web_search"}
				}
				setup.body["tools"] = []any{search, map[string]any{"type": "deepseek_read_url"}}
				setup.body["max_tool_calls"] = 3
				run := runDeepSeekFailOpen(t, setup)
				require.NoError(t, run.err)
				require.False(t, run.handled, "the request is answered on the ordinary path")
				require.False(t, gjson.GetBytes(run.forward, "tools").Exists(), "%s", run.forward)
				require.False(t, gjson.GetBytes(run.forward, "max_tool_calls").Exists())
				require.Contains(t, string(run.forward), deepSeekToolsUnavailableNotice)
			})
		}
	}
	// A tool the client forced is no longer forced once it is gone; a client
	// function stays.
	run := runDeepSeekFailOpen(t, deepSeekFailOpenSetup{protocol: "chat", tools: enabled, extra: map[string]any{}, body: map[string]any{
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, map[string]any{"type": "deepseek_web_search"}},
		"tool_choice": map[string]any{"type": "deepseek_web_search"},
	}})
	require.NoError(t, run.err)
	require.Equal(t, "auto", gjson.GetBytes(run.forward, "tool_choice").String())
	require.Equal(t, "lookup", gjson.GetBytes(run.forward, "tools.0.function.name").String())
	require.Len(t, gjson.GetBytes(run.forward, "tools").Array(), 1)
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
