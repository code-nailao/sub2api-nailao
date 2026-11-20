//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// deepSeekInChatRun drives one request through the hosted tool hook against a
// Chat Completions DeepSeek upstream that calls the gateway's search function
// once and then answers.
type deepSeekInChatRun struct {
	result      *OpenAIForwardResult
	handled     bool
	err         error
	forward     []byte
	firstTurn   []byte
	toolCalls   int
	journal     *deepSeekJournalFixture
	clientReply string
}

// With nativeResponses the account speaks the provider's native Responses
// protocol, the one the removed native tool branch used to require.
func runDeepSeekInChat(t *testing.T, protocol string, request map[string]any, extra map[string]any, nativeResponses ...bool) deepSeekInChatRun {
	t.Helper()
	gin.SetMode(gin.TestMode)
	price, groupID := 6.0, int64(1)
	key := &APIKey{ID: 4, UserID: 2, GroupID: &groupID, User: &User{ID: 2, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: &price}}}
	if extra == nil {
		extra = map[string]any{"deepseek_tools_enabled": true}
	}
	account := &Account{ID: 18, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example/v1", "api_protocol": "chat_completions"}, Extra: extra}
	native := len(nativeResponses) > 0 && nativeResponses[0]
	if native {
		account.Credentials["api_protocol"] = APIProtocolResponses
		require.True(t, account.UsesNativeCNResponses())
	}
	run := deepSeekInChatRun{journal: &deepSeekJournalFixture{}}
	models := 0
	upstream := deepSeekHTTPFixture{fn: func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		if strings.HasSuffix(req.URL.Path, "/tools/search") {
			run.toolCalls++
			return deepSeekFixtureResponse(`{"results":[{"title":"A source","url":"https://example.com","snippet":"Evidence"}]}`), nil
		}
		models++
		if native {
			require.True(t, strings.HasSuffix(req.URL.Path, "/responses"), req.URL.Path)
			if models == 1 {
				run.firstTurn = body
				name := ""
				for _, tool := range gjson.GetBytes(body, "tools").Array() {
					if n := tool.Get("name").String(); strings.HasPrefix(n, "sources_web_search_") {
						name = n
					}
				}
				require.NotEmpty(t, name, "the gateway's own search function is offered: %s", body)
				return deepSeekFixtureResponse(fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":%q,"arguments":"{\"query\":\"Python\"}","status":"completed"}],"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`, name)), nil
			}
			return deepSeekFixtureResponse(`{"id":"resp_2","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Final answer https://example.com","annotations":[]}]}],"usage":{"input_tokens":120,"output_tokens":10,"total_tokens":130}}`), nil
		}
		require.True(t, strings.HasSuffix(req.URL.Path, "/chat/completions"), req.URL.Path)
		message := `"content":"Final answer https://example.com"`
		finish := "stop"
		if models == 1 {
			run.firstTurn = body
			name := ""
			for _, tool := range gjson.GetBytes(body, "tools").Array() {
				if n := tool.Get("function.name").String(); strings.HasPrefix(n, "sources_web_search_") {
					name = n
				}
			}
			require.NotEmpty(t, name, "the gateway's own search function is offered: %s", body)
			message = fmt.Sprintf(`"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":"{\"query\":\"Python\"}"}}]`, name)
			finish = "tool_calls"
		}
		return deepSeekFixtureResponse(fmt.Sprintf(`{"id":"chatcmpl-%d","object":"chat.completion","created":123,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant",%s},"finish_reason":%q}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`, models, message, finish)), nil
	}}
	cfg := deepSeekChatFallbackTestConfig()
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}, billingService: newTestBillingService(), usageBillingRepo: run.journal, billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
	body := map[string]any{"model": "deepseek-flash"}
	for k, v := range request {
		body[k] = v
	}
	switch protocol {
	case "responses":
		body["input"] = "Find Python"
	case "messages":
		body["max_tokens"] = 128
		fallthrough
	default:
		body["messages"] = []any{map[string]any{"role": "user", "content": "Find Python"}}
	}
	wire, err := json.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(string(wire)))
	c.Set("api_key", key)
	SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})
	run.result, run.forward, run.handled, run.err = svc.tryForwardDeepSeekTools(context.Background(), c, account, wire, protocol, "")
	run.clientReply = recorder.Body.String()
	return run
}

func deepSeekProviderTools() []any {
	return []any{map[string]any{"type": "deepseek_web_search"}, map[string]any{"type": "deepseek_read_url"}}
}

// The provider's own tool types are executed and billed by the gateway like
// web_search and web_fetch, in all three protocols.
func TestDeepSeekProviderStyleToolsRunInTheHostedLoop(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			run := runDeepSeekInChat(t, protocol, map[string]any{"tools": deepSeekProviderTools()}, nil)
			require.NoError(t, run.err)
			require.True(t, run.handled)
			require.NotContains(t, string(run.firstTurn), "deepseek_web_search", "the provider never sees its own tool types")
			require.NotContains(t, string(run.firstTurn), "deepseek_read_url")
			require.Equal(t, 1, run.toolCalls, "the search runs through the standalone tool endpoint")
			require.Equal(t, 1, run.result.DeepSeekTools.SearchCount)
			last := run.journal.checkpoints[len(run.journal.checkpoints)-1]
			require.InDelta(t, .006, last.Usage.BillingSnapshot.DeepSeekTools.Cost(), 1e-12)
			require.Contains(t, run.clientReply, "Final answer")
		})
	}
}

// Forcing a provider-style tool forces the gateway's function, as forcing
// web_search does.
func TestDeepSeekForcedProviderStyleToolBecomesTheGatewayFunction(t *testing.T) {
	for protocol, choice := range map[string]any{
		"chat":      map[string]any{"type": "deepseek_web_search"},
		"responses": map[string]any{"type": "deepseek_web_search"},
		"messages":  map[string]any{"type": "tool", "name": "deepseek_web_search"},
	} {
		t.Run(protocol, func(t *testing.T) {
			run := runDeepSeekInChat(t, protocol, map[string]any{"tools": deepSeekProviderTools(), "tool_choice": choice}, nil)
			require.NoError(t, run.err)
			require.True(t, run.handled)
			forced := gjson.GetBytes(run.firstTurn, "tool_choice.function.name").String()
			require.True(t, strings.HasPrefix(forced, "sources_web_search_"), "first turn forces the gateway's search: %s", gjson.GetBytes(run.firstTurn, "tool_choice").Raw)
		})
	}
}

// A saved deepseek_native_tools_enabled is accepted and has no effect: such an
// account runs the hosted loop like any other.
func TestDeepSeekNativeToolsSettingIsIgnored(t *testing.T) {
	extra := map[string]any{"deepseek_tools_enabled": true, "deepseek_native_tools_enabled": true}
	require.NoError(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, extra))
	require.Error(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"deepseek_tools_enabled": true, "deepseek_native_tools_enabled": "yes"}))
	run := runDeepSeekInChat(t, "responses", map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}, extra, true)
	require.NoError(t, run.err)
	require.True(t, run.handled)
	require.NotContains(t, string(run.firstTurn), `"web_search"`, "the provider receives the gateway's function, not its native tool")
	require.Equal(t, 1, run.toolCalls, "the search ran through the standalone tool endpoint")
	require.Equal(t, 1, run.result.DeepSeekTools.SearchCount)
	require.Contains(t, run.clientReply, "Final answer")
}

// Without the hosted loop the provider's own tool types are removed and every
// other byte is forwarded as sent; web_search stays for the ordinary path.
func TestDeepSeekProviderStyleToolsAreRemovedWithoutTheHostedLoop(t *testing.T) {
	sent := `{"model":"deepseek-flash",  "seed": 12345678901234567890, "temperature": 0.70,
 "tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},{"type":"deepseek_web_search"},{"type":"web_search"},{"type":"deepseek_read_url"}],
 "messages":[{"role":"user","content":"hi"}]}`
	forward, removed, err := withoutDeepSeekWebTools([]byte(sent), "chat", deepSeekProviderTool, true)
	require.NoError(t, err)
	require.True(t, removed)
	got := string(forward)
	require.NotContains(t, got, "deepseek_web_search")
	require.NotContains(t, got, "deepseek_read_url")
	for _, untouched := range []string{`{"model":"deepseek-flash",  "seed": 12345678901234567890, "temperature": 0.70,`, `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`, `{"type":"web_search"}`, `{"role":"user","content":"hi"}`} {
		require.Contains(t, got, untouched)
	}
	require.Len(t, gjson.Get(got, "tools").Array(), 2)
	require.Equal(t, deepSeekToolsUnavailableNotice, gjson.Get(got, "messages.1.content").String())
	require.Equal(t, "system", gjson.Get(got, "messages.1.role").String())

	// A request with nothing to remove is returned byte for byte.
	same, removed, err := withoutDeepSeekWebTools([]byte(`{"tools":[{"type":"web_search"}], "x":1}`), "chat", deepSeekProviderTool, true)
	require.NoError(t, err)
	require.False(t, removed)
	require.Equal(t, `{"tools":[{"type":"web_search"}], "x":1}`, string(same))
}

func TestDeepSeekRemovedToolChoiceHandling(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, sent string
		check                func(t *testing.T, got string)
	}{
		{"tools_emptied", "chat", `{"tools":[{"type":"deepseek_web_search"}],"tool_choice":{"type":"deepseek_web_search"},"messages":[]}`, func(t *testing.T, got string) {
			require.False(t, gjson.Get(got, "tools").Exists())
			require.False(t, gjson.Get(got, "tool_choice").Exists())
		}},
		{"forced_removed_tool", "chat", `{"tools":[{"type":"function","function":{"name":"f"}},{"type":"deepseek_read_url"}],"tool_choice":{"type":"deepseek_read_url"},"messages":[]}`, func(t *testing.T, got string) {
			require.Equal(t, "auto", gjson.Get(got, "tool_choice").String())
		}},
		{"forced_removed_tool_messages", "messages", `{"tools":[{"name":"f","input_schema":{}},{"type":"deepseek_web_search","name":"deepseek_web_search"}],"tool_choice":{"type":"tool","name":"deepseek_web_search"},"system":"be brief","messages":[]}`, func(t *testing.T, got string) {
			require.JSONEq(t, `{"type":"auto"}`, gjson.Get(got, "tool_choice").Raw)
			require.Equal(t, "be brief\n\n"+deepSeekToolsUnavailableNotice, gjson.Get(got, "system").String())
		}},
		{"client_function_forced_stays", "chat", `{"tools":[{"type":"function","function":{"name":"f"}},{"type":"deepseek_web_search"}],"tool_choice":{"type":"function","function":{"name":"f"}},"messages":[]}`, func(t *testing.T, got string) {
			require.Equal(t, "f", gjson.Get(got, "tool_choice.function.name").String())
		}},
		{"responses_string_input", "responses", `{"tools":[{"type":"deepseek_web_search"}],"input":"hi"}`, func(t *testing.T, got string) {
			require.Equal(t, "hi", gjson.Get(got, "input.0.content").String())
			require.Equal(t, deepSeekToolsUnavailableNotice, gjson.Get(got, "input.1.content").String())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forward, removed, err := withoutDeepSeekWebTools([]byte(tc.sent), tc.protocol, deepSeekProviderTool, true)
			require.NoError(t, err)
			require.True(t, removed)
			require.True(t, gjson.ValidBytes(forward), "%s", forward)
			tc.check(t, string(forward))
		})
	}
}

// End to end on the Chat Completions path, which forwards non-function tools
// as they are: with the tools off for the group, the upstream never sees the
// provider's tool types; with tool_choice none there is no notice.
func TestDeepSeekChatPathDropsProviderToolsWhenToolsAreOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		choice  string
		notice  bool
	}{{"tools_off", false, "", true}, {"tool_choice_none", true, `,"tool_choice":"none"`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			price, groupID := 6.0, int64(7)
			key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: tc.enabled, FetchPricePer1K: &price}}}
			account := deepSeekForcedToolChoiceAccount()
			account.GroupIDs = []int64{groupID}
			body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"deepseek_web_search"},{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]` + tc.choice + `}`)
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			c.Set("api_key", key)
			upstream := newOKChatCompletionsUpstream("rid_ds_provider_tools", deepSeekChatFallbackOKBody)
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}}
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.DeepSeekTools, "billed as an ordinary request")
			require.NotContains(t, string(upstream.lastBody), "deepseek_web_search")
			require.Equal(t, "lookup", gjson.GetBytes(upstream.lastBody, "tools.0.function.name").String())
			require.Equal(t, tc.notice, strings.Contains(string(upstream.lastBody), deepSeekToolsUnavailableNotice))
		})
	}
}

// Case C end to end: a declared provider tool whose setup fails is answered as
// an ordinary request without web tools and with the notice.
func TestDeepSeekChatPathAnswersWhenDeclaredToolsCannotBeSetUp(t *testing.T) {
	price, groupID := 6.0, int64(7)
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: &price}}}
	account := deepSeekForcedToolChoiceAccount()
	account.GroupIDs = []int64{groupID}
	account.Extra = map[string]any{} // the model account supplies no tools
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"deepseek_web_search"},{"type":"web_search"}],"tool_choice":"required"}`)
	c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
	c.Set("api_key", key)
	upstream := newOKChatCompletionsUpstream("rid_ds_setup_failed", deepSeekChatFallbackOKBody)
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}}
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.DeepSeekTools)
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools").Exists(), "%s", upstream.lastBody)
	require.False(t, gjson.GetBytes(upstream.lastBody, "tool_choice").Exists())
	require.Contains(t, string(upstream.lastBody), deepSeekToolsUnavailableNotice)
}

type deepSeekOrdinaryChatRun struct {
	result   *OpenAIForwardResult
	err      error
	sent     []byte
	calls    int
	recorder *httptest.ResponseRecorder
	billing  *openAIRecordUsageBillingRepoStub
}

// Exercise forwarding and the same usage recording the handler performs after
// success. All upstream and persistence operations use in-memory test doubles.
func runDeepSeekOrdinaryChat(t *testing.T, body []byte, enabled bool) deepSeekOrdinaryChatRun {
	t.Helper()
	run := deepSeekOrdinaryChatRun{recorder: httptest.NewRecorder(), billing: &openAIRecordUsageBillingRepoStub{}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{inserted: true}, run.billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	multiplier := svc.cfg.Default.RateMultiplier
	svc.cfg = deepSeekChatFallbackTestConfig()
	svc.cfg.Default.RateMultiplier = multiplier
	account := deepSeekForcedToolChoiceAccount()
	group := &Group{ID: account.ID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: multiplier, DeepSeekTools: DeepSeekToolsConfig{Enabled: enabled}}
	user := &User{ID: account.ID}
	key := &APIKey{ID: account.ID, UserID: user.ID, User: user, GroupID: &group.ID, Group: group}
	account.GroupIDs = []int64{group.ID}
	svc.accountRepo = deepSeekAccountFixture{account: account}
	svc.httpUpstream = deepSeekHTTPFixture{fn: func(req *http.Request) (*http.Response, error) {
		run.calls++
		require.True(t, strings.HasSuffix(req.URL.Path, "/chat/completions"))
		var err error
		run.sent, err = io.ReadAll(req.Body)
		require.NoError(t, err)
		response := deepSeekFixtureModelResponse(t, gjson.GetBytes(body, "stream").Bool(), deepSeekChatFallbackOKBody)
		response.Header.Set("X-Request-ID", t.Name())
		return response, nil
	}}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(run.recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	c.Set("api_key", key)
	run.result, run.err = svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
	if run.err == nil {
		require.NoError(t, svc.RecordUsage(c.Request.Context(), &OpenAIRecordUsageInput{Result: run.result, APIKey: key, User: user, Account: account}))
	}
	return run
}

func TestDeepSeekChatFiltersDisabledGroup(t *testing.T) {
	const lookup = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`
	const namedSearch = `{"type":"function","function":{"name":"deepseek_web_search"}}`
	const legacySearchChoice = `{"type":"tool", "name":"deepseek_web_search"}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			ordinary := runDeepSeekOrdinaryChat(t, []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t}`, stream)), false)
			require.NoError(t, ordinary.err)
			require.Equal(t, 1, ordinary.calls)
			require.Equal(t, 1, ordinary.billing.calls)
			require.Positive(t, ordinary.billing.lastCmd.BalanceCost)
			for _, tc := range []struct {
				name, tools, choice, wantTools, wantChoice string
			}{
				{"future_types", `{"type":"web_search_future"},{"type":"web_fetch_preview_future"},{"type":"deepseek_web_search_future"},{"type":"deepseek_read_url_preview_future"},` + lookup, `"auto"`, `[` + lookup + `]`, `"auto"`},
				{"hyphen_suffix", `{"type":"web_search-v2099"},` + lookup, `"auto"`, `[` + lookup + `]`, `"auto"`},
				{"dot_suffix", `{"type":"web_fetch.preview"},` + lookup, `"auto"`, `[` + lookup + `]`, `"auto"`},
				{"prefix_without_separator", `{"type":"web_search2099"},{"type":"web_fetch2099"},` + lookup, `"auto"`, `[` + lookup + `]`, `"auto"`},
				{"underscore_suffix", `{"type":"web_search_preview_2025_03_11"},` + lookup, `"auto"`, `[` + lookup + `]`, `"auto"`},
				{"legacy_named_choice", `{"type":"deepseek_web_search"},` + lookup, `{"type":"tool","name":"deepseek_web_search"}`, `[` + lookup + `]`, `"auto"`},
				{"legacy_named_alias", `{"type":"deepseek_web_search","name":"search_alias"},` + lookup, `{"type":"tool","name":"search_alias"}`, `[` + lookup + `]`, `"auto"`},
				{"legacy_choice_of_retained_tool", `{"type":"deepseek_web_search"},` + lookup, `{"type":"tool","name":"lookup"}`, `[` + lookup + `]`, `{"type":"tool","name":"lookup"}`},
				{"legacy_kept_function_named_after_provider_tool", `{"type":"web_fetch"},` + namedSearch, legacySearchChoice, `[` + namedSearch + `]`, legacySearchChoice},
				{"legacy_kept_function_named_after_removed_alias", `{"type":"web_fetch","name":"lookup"},` + lookup, `{"type":"tool","name":"lookup"}`, `[` + lookup + `]`, `{"type":"tool","name":"lookup"}`},
				{"choice_by_family", `{"type":"web_search_preview_future"},` + lookup, `{"type":"web_search"}`, `[` + lookup + `]`, `"auto"`},
				{"removed_function_choice", `{"type":"web_fetch"},` + lookup, `{"type":"function","function":{"name":"missing"}}`, `[` + lookup + `]`, `"auto"`},
				{"retained_function_choice", `{"type":"web_search"},{"type":"function","function":{"name":"web_search"}}`, `{"type":"function", "function":{"name":"web_search"}}`, `[{"type":"function","function":{"name":"web_search"}}]`, `{"type":"function", "function":{"name":"web_search"}}`},
				{"required_without_survivors", `{"type":"deepseek_web_search"}`, `"required"`, "", ""},
				{"no_notice_when_none", `{"type":"web_search"}`, `"none"`, "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,"tools":[%s],"tool_choice":%s}`, stream, tc.tools, tc.choice))
					run := runDeepSeekOrdinaryChat(t, body, false)
					require.NoError(t, run.err)
					require.NotNil(t, run.result)
					require.Nil(t, run.result.DeepSeekTools)
					require.Equal(t, http.StatusOK, run.recorder.Code)
					require.Equal(t, stream, run.result.Stream)
					require.Equal(t, 1, run.calls)
					require.Equal(t, tc.wantTools, gjson.GetBytes(run.sent, "tools").Raw)
					require.Equal(t, tc.wantChoice, gjson.GetBytes(run.sent, "tool_choice").Raw)
					require.Equal(t, tc.choice != `"none"`, strings.Contains(string(run.sent), deepSeekToolsUnavailableNotice))
					require.Equal(t, ordinary.billing.calls, run.billing.calls)
					require.Equal(t, ordinary.billing.lastCmd.BalanceCost, run.billing.lastCmd.BalanceCost)
					require.Equal(t, ordinary.billing.lastCmd.APIKeyQuotaCost, run.billing.lastCmd.APIKeyQuotaCost)
					if stream {
						require.Contains(t, run.recorder.Body.String(), "data: [DONE]")
					}
				})
			}
		})
	}
}

func TestDeepSeekChatFilterHandlesInputShapeOnFixedChat(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, input, wantInput string }{
			{"string", `"hello\u0020world"`, `[{"content":"hello\u0020world","role":"user"},` + deepSeekChatNoticeMessage + `]`},
			{"array", `[{ "role":"user", "content":"hello" }]`, `[{ "role":"user", "content":"hello" },` + deepSeekChatNoticeMessage + `]`},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","stream":%t,"input":%s,"tools":[{"type":"web_search"}],"tool_choice":"required"}`, stream, tc.input))
				run := runDeepSeekOrdinaryChat(t, body, false)
				require.NoError(t, run.err)
				require.Equal(t, 1, run.calls)
				require.Equal(t, 1, run.billing.calls)
				require.Nil(t, run.result.DeepSeekTools)
				require.False(t, gjson.GetBytes(run.sent, "tools").Exists())
				require.False(t, gjson.GetBytes(run.sent, "tool_choice").Exists())
				require.False(t, gjson.GetBytes(run.sent, "messages").Exists())
				require.Equal(t, tc.wantInput, gjson.GetBytes(run.sent, "input").Raw)
			})
		}
	}
}

func TestDeepSeekChatFilterRejectsDuplicateToolKeys(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, fields string }{
			{"tools", `"tools":[],"tools":[{"type":"web_search"}]`},
			{"escaped_tools", `"tools":[],"\u0074ools":[{"type":"web_fetch"}]`},
			{"tool_type", `"tools":[{"type":"function","type":"web_search"}]`},
			{"schema_property", `"tools":[{"type":"function","function":{"parameters":{"properties":{"term":{},"\u0074erm":{}}}}}]`},
			{"tool_choice", `"tools":[{"type":"web_search"}],"tool_choice":"none","tool_choice":{"type":"function","function":{"name":"web_search"}}`},
			{"tool_choice_without_tools", `"tool_choice":"none","tool_choic\u0065":"auto"`},
			{"nested_name", `"tools":[{"type":"web_search"},{"type":"function","function":{"name":"lookup"}}],"tool_choice":{"type":"function","function":{"name":"lookup","name":"web_search"}}`},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,%s}`, stream, tc.fields))
				started := time.Now()
				run := runDeepSeekOrdinaryChat(t, body, false)
				require.Less(t, time.Since(started), 2*time.Second)
				require.ErrorContains(t, run.err, "duplicate")
				requireDeepSeekChatFilterRejected(t, run, "duplicate")
			})
		}
	}
}

func TestDeepSeekChatFilterAddsNoticeToLastMessages(t *testing.T) {
	const firstMessages = `"messages":[]`
	const userMessage = `{"role":"user","content":"hello"}`
	for _, stream := range []bool{false, true} {
		for _, key := range []string{`"messages"`, `"\u006dessages"`} {
			t.Run(fmt.Sprintf("stream=%t/key=%s", stream, key), func(t *testing.T) {
				prefix := fmt.Sprintf(`{"model":"deepseek-flash","stream":%t,"stream_options":{"include_usage":true},`, stream)
				last := key + `:[` + userMessage
				body := []byte(prefix + firstMessages + `,"tools":[{"type":"web_search"}],` + last + `]}`)
				run := runDeepSeekOrdinaryChat(t, body, false)
				require.NoError(t, run.err)
				require.Equal(t, 1, run.calls)
				require.Equal(t, 1, run.billing.calls)
				require.Equal(t, prefix+firstMessages+`,`+last+`,`+deepSeekChatNoticeMessage+`]}`, string(run.sent))
				var upstream struct {
					Messages []json.RawMessage `json:"messages"`
				}
				require.NoError(t, json.Unmarshal(run.sent, &upstream))
				require.Len(t, upstream.Messages, 2)
				require.JSONEq(t, userMessage, string(upstream.Messages[0]))
				require.JSONEq(t, deepSeekChatNoticeMessage, string(upstream.Messages[1]))
			})
		}
	}
}

func TestDeepSeekChatFilterAllowsDuplicateMessagesWithoutNotice(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const prefix = `{"model":"deepseek-flash","stream":%t,"stream_options":{"include_usage":true},"messages":[],`
			const lastMessages = `"messages":[{"role":"user","content":"hello"}]}`
			unchanged := []byte(fmt.Sprintf(prefix+`"tools":[{"type":"function","function":{"name":"lookup"}}],`+lastMessages, stream))
			withoutWeb := runDeepSeekOrdinaryChat(t, unchanged, false)
			require.NoError(t, withoutWeb.err)
			require.Equal(t, 1, withoutWeb.calls)
			require.Equal(t, unchanged, withoutWeb.sent)

			declined := []byte(fmt.Sprintf(prefix+`"tools":[{"type":"web_search"}],"tool_choice":"none",`+lastMessages, stream))
			withoutNotice := runDeepSeekOrdinaryChat(t, declined, false)
			require.NoError(t, withoutNotice.err)
			require.Equal(t, 1, withoutNotice.calls)
			require.Equal(t, fmt.Sprintf(prefix+lastMessages, stream), string(withoutNotice.sent))
		})
	}
}

func requireDeepSeekChatFilterRejected(t *testing.T, run deepSeekOrdinaryChatRun, reason string) {
	t.Helper()
	require.Nil(t, run.result)
	require.Equal(t, http.StatusBadRequest, run.recorder.Code)
	response := run.recorder.Body.String()
	require.Equal(t, "invalid_request_error", gjson.Get(response, "error.type").String())
	require.Contains(t, gjson.Get(response, "error.message").String(), reason)
	require.Zero(t, run.calls, "rejected requests must not reach the upstream")
	require.Empty(t, run.sent)
	require.Zero(t, run.billing.calls, "rejected requests must not be billed")
	require.Nil(t, run.billing.lastCmd)
}

func TestDeepSeekChatEnabledGroupBypassesStrictFilter(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,"stream_options":{"include_usage":true},"tool_choice":"none","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","type":"object"}}}]}`, stream))
			run := runDeepSeekOrdinaryChat(t, body, true)
			require.NoError(t, run.err)
			require.Equal(t, 1, run.calls)
			require.Equal(t, 1, run.billing.calls)
			require.Equal(t, body, run.sent)
		})
	}
}

func TestDeepSeekChatWithNoWebToolsKeepsBytes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fields := range []string{
			`"metadata":{"labels":{"x":1,"x":2},"large":123456789012345678901234567890}`,
			`"tools": null, "tool_choice":{"type":"auto","data":{"x":1,"x":2}}`,
			`"tools":{"type":"web_search"}, "meta": "{\"tools\":[{}]}"`,
			`"tools": [ {"type":"function", "function":{"name":"web_search_preview_future"}}, {"type":"client_tool"} ]`,
		} {
			body := []byte(fmt.Sprintf(`{ "model":"deepseek-flash", "messages":[], "stream":%t,"stream_options":{"include_usage":true}, %s }`, stream, fields))
			run := runDeepSeekOrdinaryChat(t, body, false)
			require.NoError(t, run.err)
			require.Equal(t, 1, run.calls)
			require.Equal(t, 1, run.billing.calls)
			require.Equal(t, body, run.sent)
		}
	}
}

func TestDeepSeekChatFilterBypassesOtherProtocols(t *testing.T) {
	account := deepSeekForcedToolChoiceAccount()
	key := &APIKey{Group: &Group{ID: account.ID, Platform: PlatformDeepseek}}
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig()}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, protocol, input, accountProtocol string }{
			{"responses", "responses", `"input":[]`, APIProtocolChatCompletions},
			{"messages", "messages", `"messages":[]`, APIProtocolChatCompletions},
			{"responses_shape_fixed_responses", "chat", `"input":[]`, APIProtocolResponses},
			{"responses_shape_adaptive", "chat", `"input":[]`, APIProtocolAdaptive},
		} {
			t.Run(fmt.Sprintf("%t/%s", stream, tc.name), func(t *testing.T) {
				account.Credentials["api_protocol"] = tc.accountProtocol
				body := []byte(fmt.Sprintf(`{"model":"deepseek-flash", "stream":%t,"tools":[],"tools":[{"type":"web_search"}],"tool_choice":{"type":"function","function":{"name":"web_search"}},%s}`, stream, tc.input))
				c := newDeepSeekForcedToolChoiceContext(t, "/v1/test", body)
				c.Set("api_key", key)
				result, forward, handled, err := svc.tryForwardDeepSeekTools(c.Request.Context(), c, account, body, tc.protocol, "")
				require.NoError(t, err)
				require.False(t, handled)
				require.Nil(t, result)
				require.Equal(t, body, forward)
			})
		}
	}
}

// A versioned type recognized by the disabled-group filter must not become
// eligible for hosted execution in an enabled group with auto tools off.
func TestDeepSeekChatFilterLeavesHostedEligibilityAlone(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			const typ = "web_search_preview_2025_03_11"
			run := runDeepSeekInChat(t, protocol, map[string]any{"tools": []any{map[string]any{"type": typ}}}, nil)
			require.NoError(t, run.err)
			require.False(t, run.handled)
			require.Nil(t, run.result)
			require.Zero(t, run.toolCalls)
			require.Empty(t, run.journal.checkpoints)
			require.Equal(t, typ, gjson.GetBytes(run.forward, "tools.0.type").String())
		})
	}
}

func TestDeepSeekChatFilterToolListBounds(t *testing.T) {
	const client = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`
	const web = `{"type":"web_search"}`
	const deadline = 2 * time.Second
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Run("many_removals", func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,"tools":[%s%s]}`, stream, strings.Repeat(web+",", 20_000), client))
				started := time.Now()
				run := runDeepSeekOrdinaryChat(t, body, false)
				require.Less(t, time.Since(started), deadline, "tool filtering must fit the time bound")
				require.NoError(t, run.err)
				require.Equal(t, 1, run.calls)
				require.Equal(t, 1, run.billing.calls)
				require.Equal(t, "["+client+"]", gjson.GetBytes(run.sent, "tools").Raw)
			})
			entries := strings.Repeat(`null,`, deepSeekMaxRequestTools) + `null`
			for _, tc := range []struct{ name, fields string }{
				{"tools", `"tools":[` + entries + `]`},
				{"choice_array", `"tools":[` + web + `],"tool_choice":{"options":[` + entries + `]}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,%s}`, stream, tc.fields))
					started := time.Now()
					run := runDeepSeekOrdinaryChat(t, body, false)
					require.Less(t, time.Since(started), deadline, "tool-count validation must fit the time bound")
					require.ErrorContains(t, run.err, "tool count")
					requireDeepSeekChatFilterRejected(t, run, "tool count")
				})
			}
		})
	}
}

func TestDeepSeekChatFilterScanBounds(t *testing.T) {
	const deadline = 2 * time.Second
	for _, tc := range []struct {
		name, metadata string
		cancel         bool
	}{
		{"already_canceled", strings.Repeat(`{"next":`, 8000) + `"` + strings.Repeat("x", 8<<20) + `"` + strings.Repeat("}", 8000), true},
		{"depth_limit", strings.Repeat(`{"next":`, 256) + `null` + strings.Repeat("}", 256), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"tools":[{"type":"web_search","metadata":` + tc.metadata + `}],"messages":[],"model":"deepseek-flash"}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			account := deepSeekForcedToolChoiceAccount()
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			c.Request = c.Request.WithContext(ctx)
			c.Set("api_key", &APIKey{Group: &Group{ID: account.ID, Platform: PlatformDeepseek}})
			svc := &OpenAIGatewayService{}
			type outcome struct {
				result  *OpenAIForwardResult
				handled bool
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				result, _, handled, err := svc.tryForwardDeepSeekTools(ctx, c, account, body, "chat", "")
				done <- outcome{result, handled, err}
			}()
			timer := time.NewTimer(deadline)
			defer timer.Stop()
			select {
			case got := <-done:
				require.Nil(t, got.result)
				require.True(t, got.handled)
				if tc.cancel {
					require.ErrorIs(t, got.err, context.Canceled)
				} else {
					require.ErrorContains(t, got.err, "nesting")
					require.Equal(t, http.StatusBadRequest, c.Writer.Status())
				}
			case <-timer.C:
				t.Fatal("Chat JSON validation exceeded its time bound")
			}
		})
	}
}

func TestDeepSeekChatFilterPreservesContextError(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if cause == context.DeadlineExceeded {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()

			body := []byte(`{"model":"deepseek-flash","messages":[],"tools":[{"type":"web_search"}]}`)
			account := deepSeekForcedToolChoiceAccount()
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			c.Request = c.Request.WithContext(ctx)
			c.Set("api_key", &APIKey{Group: &Group{Platform: PlatformDeepseek}})
			svc := &OpenAIGatewayService{}
			result, err := svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
			require.Nil(t, result)
			require.ErrorIs(t, err, cause)
			require.EqualError(t, err, cause.Error())
			require.False(t, c.Writer.Written())
			require.ErrorIs(t, err, ErrDeepSeekChatToolValidation)
		})
	}
}

type deepSeekCancelDuringScan struct {
	context.Context
	cancel        context.CancelFunc
	checks, after int
}

func (c *deepSeekCancelDuringScan) Err() error {
	c.checks++
	if c.checks == c.after {
		c.cancel()
	}
	return c.Context.Err()
}

func runDeepSeekChatFilter(ctx context.Context, body []byte) ([]byte, error) {
	top, err := scanDeepSeekChatEnvelope(ctx, body)
	if err != nil {
		return nil, err
	}
	return filterDeepSeekChatWebTools(ctx, body, top)
}

func TestDeepSeekChatJSONCancellationDuringScan(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		duringTools  bool
	}{
		{"opaque_string", `"metadata":"` + strings.Repeat("x", 8<<20) + `","tools":[]`, false},
		{"tool_string", `"tools":[{"type":"web_search","description":"` + strings.Repeat("x", 8<<20) + `"}]`, true},
		{"tool_array", `"tools":[` + strings.Repeat(`{"type":"web_search"},`, 20_000) + `null]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := []byte(`{` + tc.fields + `}`)
			probe := &deepSeekCancelDuringScan{Context: ctx, cancel: cancel, after: 16}
			if tc.duringTools {
				// Let the envelope scan finish, then cancel during the
				// strict decoder's refills or token reads.
				probe.after += len(body) / deepSeekJSONChunk
			}
			started := time.Now()
			out, err := runDeepSeekChatFilter(probe, body)
			require.Less(t, time.Since(started), 2*time.Second)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, out)
		})
	}
}

func TestDeepSeekChatJSONDepthBoundary(t *testing.T) {
	for _, array := range []bool{false, true} {
		for _, depth := range []int{deepSeekChatJSONMaxDepth, deepSeekChatJSONMaxDepth + 1} {
			t.Run(fmt.Sprintf("array=%t/depth=%d", array, depth), func(t *testing.T) {
				open, close := `{"next":`, `}`
				if array {
					open, close = `[`, `]`
				}
				body := []byte(`{"tools":[],"tool_choice":` + strings.Repeat(open, depth-1) + `null` + strings.Repeat(close, depth-1) + `}`)
				started := time.Now()
				out, err := runDeepSeekChatFilter(context.Background(), body)
				require.Less(t, time.Since(started), 2*time.Second)
				if depth == deepSeekChatJSONMaxDepth {
					require.NoError(t, err)
					require.Equal(t, body, out)
				} else {
					require.ErrorContains(t, err, "nesting")
				}
			})
		}
	}
}

func TestDeepSeekChatJSONKeepsObjectScopesAndDecodedKeys(t *testing.T) {
	for _, tc := range []struct{ name, body, failure string }{
		{"siblings", `{"tools":[{"type":"function","function":{"name":"lookup"},"other":[{"name":"lookup"},{"name":"lookup"}]}],"seed":123456789012345678901234567890}`, ""},
		{"nested_scope", `{"tools":[{"nested":{"name":"lookup"},"name":"lookup","empty":{}}],"array":[[],{},true,false,null]}`, ""},
		{"opaque_duplicates", `{"metadata":[{"\u006eame":"first","name":"second"}],"metadata":{},"tools":[{"type":"function"}]}`, ""},
		{"escaped_duplicate", `{"tools":[{"metadata":[{"\u006eame":"lookup","name":"lookup"}]}]}`, "duplicate"},
		{"escaped_quotes", `{"tools":[],"tool_choice":{"a\"b":null,"a\u0022b":null}}`, "duplicate"},
		{"trailing_document", `{"metadata":null}{}`, "invalid"},
		{"unfinished_object", `{"metadata":`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			out, err := runDeepSeekChatFilter(context.Background(), body)
			if tc.failure == "" {
				require.NoError(t, err)
				require.Equal(t, body, out)
			} else {
				require.ErrorContains(t, err, tc.failure)
			}
		})
	}
}

func TestDeepSeekChatFilterEditsOnlySelectedSpans(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"leading_fields_removed", ` {"tool_choice":{"type":"web_search"}, "\u0074ools":[{"type":"web_search"}], "meta":{"key":1,"key":2}} `, ` { "meta":{"key":1,"key":2},"messages":[` + deepSeekChatNoticeMessage + `]} `},
		{"nonadjacent_fields_removed", `{"tools":[{"type":"web_fetch"}], "metadata":{"literal":"[}\"","big":12345678901234567890}, "tool_choice":{"type":"function","function":{"name":"missing"}}}`, `{ "metadata":{"literal":"[}\"","big":12345678901234567890},"messages":[` + deepSeekChatNoticeMessage + `]}`},
		{"all_fields_removed", `{"tools":[{"type":"web_fetch"}], "tool_choice":"web_fetch" }`, `{ "messages":[` + deepSeekChatNoticeMessage + `]}`},
		{"retained_bytes", `{"tools" : [ 99e100 , {"type":"web_fetch_preview_future"}, {"type":"web_searchable"}, { "type": "function", "function":{"name":"web_search"}}], "messages":[{"role":"user","content":"literal \\ \" [ ]"}],"meta": {"z":1,"z":2}}`, `{"tools" : [99e100,{ "type": "function", "function":{"name":"web_search"}}], "messages":[{"role":"user","content":"literal \\ \" [ ]"},` + deepSeekChatNoticeMessage + `],"meta": {"z":1,"z":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runDeepSeekChatFilter(context.Background(), []byte(tc.body))
			require.NoError(t, err)
			require.True(t, gjson.ValidBytes(out), "%s", out)
			require.Equal(t, tc.want, string(out))
		})
	}
}
