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

// The provider-only matcher, which the ordinary path uses for Responses and
// Messages, removes the provider's own tool types and forwards every other
// byte as sent; web_search stays for those protocols' lowering.
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

// End to end on the Chat Completions path, which forwards the request without
// conversion: when the loop does not take it, web_search* and web_fetch*
// entries are removed as well, and the client's own function stays as sent.
func TestDeepSeekChatPathDropsWebToolsWhenTheLoopDoesNotTakeTheRequest(t *testing.T) {
	const lookup = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`
	const webTools = `{"type":"web_search"},{"type":"web_search_preview"},{"type":"web_search_20250305","name":"web_search"},{"type":"web_fetch"},{"type":"web_fetch_20250910","name":"web_fetch"}`
	for _, tc := range []struct {
		name    string
		enabled bool
		tools   string
		choice  string
		notice  bool
		check   func(t *testing.T, sent []byte)
	}{
		{"tools_off", false, webTools + "," + lookup, "", true, func(t *testing.T, sent []byte) {
			require.Len(t, gjson.GetBytes(sent, "tools").Array(), 1)
			require.Equal(t, lookup, gjson.GetBytes(sent, "tools.0").Raw)
		}},
		{"tool_choice_none", true, webTools + "," + lookup, `,"tool_choice":"none"`, false, func(t *testing.T, sent []byte) {
			require.Len(t, gjson.GetBytes(sent, "tools").Array(), 1)
			require.Equal(t, lookup, gjson.GetBytes(sent, "tools.0").Raw)
			require.Equal(t, "none", gjson.GetBytes(sent, "tool_choice").String())
		}},
		{"only_web_tools", false, webTools, "", true, func(t *testing.T, sent []byte) {
			require.False(t, gjson.GetBytes(sent, "tools").Exists(), "%s", sent)
			require.False(t, gjson.GetBytes(sent, "tool_choice").Exists(), "%s", sent)
		}},
		{"forced_web_search", false, `{"type":"web_search"},` + lookup, `,"tool_choice":{"type":"web_search"}`, true, func(t *testing.T, sent []byte) {
			require.Equal(t, `"auto"`, gjson.GetBytes(sent, "tool_choice").Raw)
			require.Equal(t, lookup, gjson.GetBytes(sent, "tools.0").Raw)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price, groupID := 6.0, int64(7)
			key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: tc.enabled, FetchPricePer1K: &price}}}
			account := deepSeekForcedToolChoiceAccount()
			account.GroupIDs = []int64{groupID}
			body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"tools":[` + tc.tools + `]` + tc.choice + `}`)
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			c.Set("api_key", key)
			upstream := newOKChatCompletionsUpstream("rid_ds_chat_web_tools", deepSeekChatFallbackOKBody)
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}}
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.DeepSeekTools, "billed as an ordinary request")
			sent := upstream.lastBody
			for _, typ := range []string{"web_search", "web_search_preview", "web_search_20250305", "web_fetch", "web_fetch_20250910"} {
				require.NotContains(t, string(sent), `"type":"`+typ+`"`, "%s", sent)
			}
			require.Equal(t, tc.notice, strings.Contains(string(sent), deepSeekToolsUnavailableNotice))
			tc.check(t, sent)
		})
	}
}

// When the loop does not take the request, Responses and Messages bodies, and
// a Responses-shaped body sent to the chat URL, keep their web_search entries
// byte for byte: protocol lowering drops them, and Messages search emulation
// has to see them. The provider's own types are still removed there.
func TestDeepSeekOtherProtocolsKeepWebToolsWhenTheLoopDoesNotTakeTheRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	price, groupID := 6.0, int64(7)
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: false, FetchPricePer1K: &price}}}
	account := deepSeekForcedToolChoiceAccount()
	account.GroupIDs = []int64{groupID}
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), accountRepo: deepSeekAccountFixture{account: account}}
	call := func(t *testing.T, protocol, body string) []byte {
		t.Helper()
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/test", []byte(body))
		c.Set("api_key", key)
		out, forward, handled, err := svc.tryForwardDeepSeekTools(context.Background(), c, account, []byte(body), protocol, "")
		require.NoError(t, err)
		require.False(t, handled)
		require.Nil(t, out)
		return forward
	}
	for _, tc := range []struct{ name, protocol, body string }{
		{"messages", "messages", `{"model":"deepseek-flash", "max_tokens":128,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "responses", `{"model":"deepseek-flash",  "tools":[{"type":"web_search"}],"input":"hi"}`},
		{"responses_shape_on_chat_url", "chat", `{"model":"deepseek-flash", "tools":[{"type":"web_search"}],"input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.body, string(call(t, tc.protocol, tc.body)))
		})
	}
	t.Run("messages_provider_tool_still_removed", func(t *testing.T) {
		body := `{"model":"deepseek-flash","max_tokens":128,"tools":[{"type":"web_search_20250305","name":"web_search"},{"type":"deepseek_web_search","name":"deepseek_web_search"}],"messages":[{"role":"user","content":"hi"}]}`
		got := call(t, "messages", body)
		require.NotContains(t, string(got), "deepseek_web_search")
		require.Len(t, gjson.GetBytes(got, "tools").Array(), 1)
		require.Equal(t, `{"type":"web_search_20250305","name":"web_search"}`, gjson.GetBytes(got, "tools.0").Raw)
	})
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

func TestDeepSeekChatReviewNestedForcedChoice(t *testing.T) {
	const lookup = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ordinary := runDeepSeekOrdinaryChat(t, []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t}`, stream)), false)
			require.NoError(t, ordinary.err)
			require.Equal(t, 1, ordinary.calls)
			require.Equal(t, 1, ordinary.billing.calls)
			require.Positive(t, ordinary.billing.lastCmd.BalanceCost)
			for _, tc := range []struct {
				name, web, client, choice, want string
				enabled                         bool
			}{
				{"removed_web_search", `{"type":"web_search"}`, lookup, `{"type":"function","function":{"name":"web_search"}}`, `"auto"`, false},
				{"official_search_preview", `{"type":"web_search_preview_2025_03_11"}`, lookup, `{"type":"function","function":{"name":"web_search_preview_2025_03_11"}}`, `"auto"`, false},
				{"versioned_tool_forced_by_base_name", `{"type":"web_search_preview_2025_03_11"}`, lookup, `{"type":"function","function":{"name":"web_search"}}`, `"auto"`, false},
				{"future_search_preview", `{"type":"web_search_preview_2099_01_01"}`, lookup, `{"type":"function","function":{"name":"web_search_preview_2099_01_01"}}`, `"auto"`, false},
				{"future_fetch_preview", `{"type":"web_fetch_preview_2099_01_01"}`, lookup, `{"type":"function","function":{"name":"web_fetch_preview_2099_01_01"}}`, `"auto"`, false},
				{"future_provider_search", `{"type":"deepseek_web_search_preview_future"}`, lookup, `{"type":"function","function":{"name":"deepseek_web_search_preview_future"}}`, `"auto"`, false},
				{"future_provider_fetch", `{"type":"deepseek_read_url_preview_future"}`, lookup, `{"type":"function","function":{"name":"deepseek_read_url_preview_future"}}`, `"auto"`, false},
				{"undefined_forced_function", `{"type":"web_search"}`, lookup, `{"type":"function","function":{"name":"missing"}}`, `"auto"`, false},
				{"removed_named_web_fetch", `{"type":"web_fetch","name":"fetch_page"}`, lookup, `{"type":"function","function":{"name":"fetch_page"}}`, `"auto"`, false},
				{"client_function", `{"type":"web_search"}`, lookup, `{"type":"function", "function":{"name":"lookup"}}`, `{"type":"function", "function":{"name":"lookup"}}`, false},
				{"client_function_with_web_name", `{"type":"web_search"}`, `{"type":"function","function":{"name":"web_search"}}`, `{"type":"function","function":{"name":"web_search"}}`, `{"type":"function","function":{"name":"web_search"}}`, false},
				{"versioned_tool_with_client_web_name", `{"type":"web_search_preview_2025_03_11"}`, `{"type":"function","function":{"name":"web_search"}}`, `{"type":"function","function":{"name":"web_search"}}`, `{"type":"function","function":{"name":"web_search"}}`, false},
				{"tool_choice_none", `{"type":"web_search"}`, lookup, `"none"`, `"none"`, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","messages":[],"stream":%t,"tools":[%s,%s],"tool_choice":%s}`, stream, tc.web, tc.client, tc.choice))
					run := runDeepSeekOrdinaryChat(t, body, tc.enabled)
					require.NoError(t, run.err)
					require.NotNil(t, run.result)
					require.Equal(t, http.StatusOK, run.recorder.Code)
					require.Equal(t, stream, run.result.Stream)
					require.Equal(t, 1, run.calls, "degradation must not replay the request")
					var sent struct {
						Tools []json.RawMessage `json:"tools"`
					}
					require.NoError(t, json.Unmarshal(run.sent, &sent))
					require.Len(t, sent.Tools, 1)
					require.JSONEq(t, tc.client, string(sent.Tools[0]))
					require.Equal(t, tc.want, gjson.GetBytes(run.sent, "tool_choice").Raw)
					require.Nil(t, run.result.DeepSeekTools)
					require.Equal(t, ordinary.result.Usage, run.result.Usage)
					require.Equal(t, ordinary.billing.calls, run.billing.calls)
					require.Equal(t, ordinary.billing.lastCmd.BalanceCost, run.billing.lastCmd.BalanceCost)
					require.Equal(t, ordinary.billing.lastCmd.APIKeyQuotaCost, run.billing.lastCmd.APIKeyQuotaCost)
					require.Empty(t, run.billing.lastCmd.DeepSeekToolFingerprint)
					if stream {
						require.Contains(t, run.recorder.Body.String(), "data: [DONE]")
					}
				})
			}
		})
	}
}

func TestDeepSeekChatReviewDuplicateFieldsAreRejected(t *testing.T) {
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

func TestDeepSeekChatReviewOtherProtocolsKeepDuplicateFields(t *testing.T) {
	account := deepSeekForcedToolChoiceAccount()
	key := &APIKey{Group: &Group{ID: account.ID, Platform: PlatformDeepseek}}
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig()}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, protocol, input string }{
			{"responses", "responses", `"input":[]`},
			{"messages", "messages", `"messages":[]`},
			{"responses_shape_on_chat", "chat", `"input":[]`},
		} {
			t.Run(fmt.Sprintf("%t/%s", stream, tc.name), func(t *testing.T) {
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

func TestDeepSeekChatReviewVersionedFamiliesKeepOtherProtocolBytes(t *testing.T) {
	account := deepSeekForcedToolChoiceAccount()
	svc := &OpenAIGatewayService{}
	for _, protocol := range []string{"responses", "messages"} {
		for _, typ := range []string{"web_search_preview_2025_03_11", "web_search_preview_2099_01_01", "web_fetch_preview_2099_01_01", "deepseek_web_search_preview_future", "deepseek_read_url_preview_future"} {
			t.Run(protocol+"/"+typ, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"tools":[{"type":%q}],"messages":[],"input":[]}`, typ))
				c := newDeepSeekForcedToolChoiceContext(t, "/v1/test", body)
				c.Set("api_key", &APIKey{Group: &Group{ID: account.ID, Platform: PlatformDeepseek}})
				result, forward, handled, err := svc.tryForwardDeepSeekTools(c.Request.Context(), c, account, body, protocol, "")
				require.NoError(t, err)
				require.Nil(t, result)
				require.False(t, handled)
				require.Equal(t, body, forward)
			})
		}
	}
}

func TestDeepSeekHostedToolsRecognizeFutureSearchFamilies(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, typ := range []string{"web_search_preview_2099_01_01", "deepseek_web_search_preview_future"} {
			t.Run(protocol+"/"+typ, func(t *testing.T) {
				run := runDeepSeekInChat(t, protocol, map[string]any{"tools": []any{map[string]any{"type": typ}}}, nil)
				require.True(t, run.handled)
				require.NoError(t, run.err)
				require.NotNil(t, run.result.DeepSeekTools)
				require.Equal(t, 1, run.toolCalls)
				require.Equal(t, 1, run.result.DeepSeekTools.SearchCount)
			})
		}
	}
}

func TestDeepSeekChatReviewToolListBounds(t *testing.T) {
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

func TestDeepSeekChatReviewScanBounds(t *testing.T) {
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
