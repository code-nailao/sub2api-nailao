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
