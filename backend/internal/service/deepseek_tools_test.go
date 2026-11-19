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

func TestDeepSeekToolConfiguration(t *testing.T) {
	price := 6.0
	id := int64(18)
	cfg := DeepSeekToolsConfig{Enabled: true, AutoInject: true, FetchPricePer1K: &price, AccountID: &id}
	require.NoError(t, ValidateDeepSeekToolsConfig(PlatformDeepseek, SubscriptionTypeStandard, cfg, &price))
	require.Error(t, ValidateDeepSeekToolsConfig(PlatformGrok, SubscriptionTypeStandard, cfg, &price))
	require.Error(t, ValidateDeepSeekToolsConfig(PlatformDeepseek, SubscriptionTypeStandard, cfg, nil))
	key := &APIKey{Group: &Group{Platform: PlatformDeepseek, DeepSeekTools: cfg}}
	require.True(t, key.DeepSeekToolsAutoInject())
	key.DeepSeekToolMode = "disabled"
	require.False(t, key.DeepSeekToolsAutoInject())
	key.DeepSeekToolMode = "enabled"
	key.Group.DeepSeekTools.Enabled = false
	require.False(t, key.DeepSeekToolsAutoInject())
	clone := cloneDeepSeekToolsConfig(cfg)
	*clone.FetchPricePer1K = 99
	require.Equal(t, 6.0, *cfg.FetchPricePer1K)
	require.Nil(t, key.Group.DeepSeekToolsPublic().AccountID)
}

func TestDeepSeekToolURLs(t *testing.T) {
	for _, base := range []string{"https://relay.test", "https://relay.test/v1", "https://relay.test/v1/", "https://relay.test/v1/tools"} {
		t.Run(base, func(t *testing.T) {
			target, err := BuildDeepSeekToolsURL(base, "search")
			require.NoError(t, err)
			require.Equal(t, "https://relay.test/v1/tools/search", target)
		})
	}
	target, err := BuildDeepSeekToolsURL("https://relay.test/proxy/v4", "fetch")
	require.NoError(t, err)
	require.Equal(t, "https://relay.test/proxy/v4/tools/fetch", target)
	for _, base := range []string{"file:///tmp", "https://a:b@relay.test", "https://relay.test?key=secret", "https://relay.test#x"} {
		_, err = BuildDeepSeekToolsURL(base, "search")
		require.Error(t, err)
	}
}

func TestDeepSeekToolValidationAndSuccess(t *testing.T) {
	for _, tc := range []struct {
		op, body string
		valid    bool
	}{{"search", `{"query":"Python","count":1}`, true}, {"search", `{"query":"Python","count":1.5}`, false}, {"search", `{"query":""}`, false}, {"search", `{"query":"Python","model":"free"}`, false}, {"fetch", `{"url":"https://example.com/page"}`, true}, {"fetch", `{"url":"http://127.0.0.1/admin"}`, false}, {"fetch", `{"url":"http://[::1]/"}`, false}, {"fetch", `{"url":"https://user:password@example.com/"}`, false}, {"fetch", `{"url":"http://169.254.169.254/"}`, false}, {"fetch", `{"url":"file:///etc/passwd"}`, false}} {
		t.Run(tc.body, func(t *testing.T) {
			_, err := ValidateDeepSeekToolRequest(tc.op, []byte(tc.body))
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	require.True(t, deepSeekToolSucceeded("search", 200, []byte(`{"results":[{"url":"https://example.com"}]}`)))
	require.False(t, deepSeekToolSucceeded("search", 200, []byte(`{"results":[]}`)))
	require.False(t, deepSeekToolSucceeded("search", 500, []byte(`{"results":[1]}`)))
	require.False(t, deepSeekToolSucceeded("fetch", 200, []byte(`{"content":"   "}`)))
	require.True(t, deepSeekToolSucceeded("fetch", 200, []byte(`{"content":"actual page"}`)))
}

func TestDeepSeekNativeCounts(t *testing.T) {
	var counts deepSeekCapture
	counts.observeNativeTerminal([]byte(`{"output":[{"type":"web_search_call","action":{"queries":["query","internal marker"]}}],"usage":{"server_tool_use":{"tool_search":1,"tool_fetch":2}}}`))
	require.True(t, counts.nativeObserved)
	require.Equal(t, 1, counts.nativeSearch)
	require.Equal(t, 2, counts.nativeFetch)
	counts = deepSeekCapture{}
	counts.observeNativeTerminal([]byte(`{"usage":{"web_search_requests":9},"output":[{"type":"tool_search_call"}]}`))
	require.False(t, counts.nativeObserved)
	counts = deepSeekCapture{}
	counts.observeNativeTerminal([]byte(`{"type":"response.in_progress","response":{"usage":{"server_tool_use":{"tool_search":9}}}}`))
	require.False(t, counts.nativeObserved)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	capture := newDeepSeekCapture(c.Writer, true)
	event := `data: {"type":"response.completed","response":{"usage":{"server_tool_use":{"tool_search":1,"tool_fetch":1}}}}` + "\n\n"
	for i := 0; i < 2; i++ {
		for _, ch := range []byte(event) {
			_, err := capture.Write([]byte{ch})
			require.NoError(t, err)
		}
	}
	require.True(t, capture.nativeObserved)
	require.Equal(t, 1, capture.nativeSearch)
	require.Equal(t, 1, capture.nativeFetch)
}

func TestDeepSeekHostedOwnershipAndChoice(t *testing.T) {
	for _, body := range []string{`{"tool_choice":"none"}`, `{"tool_choice":{"type":"none"}}`} {
		_, handled, err := prepareDeepSeekHosted([]byte(body), "responses", true)
		require.NoError(t, err)
		require.False(t, handled)
	}
	r, handled, err := prepareDeepSeekHosted([]byte(`{"model":"deepseek-flash","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}},{"type":"web_search"}]}`), "responses", false)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotContains(t, r.Owned, "web_search")
	calls, client := r.calls([]byte(`{"output":[{"type":"function_call","call_id":"client1","name":"web_search","arguments":"{}"}]}`))
	require.Empty(t, calls)
	require.True(t, client)
	_, _, err = prepareDeepSeekHosted([]byte(`{"tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`), "responses", false)
	require.Error(t, err)
}

func TestDeepSeekFixedToolsDoNotMultiply(t *testing.T) {
	svc := &OpenAIGatewayService{billingService: newTestBillingService()}
	for _, rate := range []float64{0, 0.35, 1, 9} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			tools := &DeepSeekToolUsage{SearchCount: 1, FetchCount: 1, SearchPrice: .006, FetchPrice: .006, Standalone: true}
			cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), &OpenAIForwardResult{Model: "deepseek-tools-search", DeepSeekTools: tools}, &APIKey{}, []string{"deepseek-tools-search"}, rate, rate, rate, rate, UsageTokens{}, "priority", nil, false, time.Now())
			require.NoError(t, err)
			require.InDelta(t, .012, cost.ActualCost, 1e-12)
			require.Zero(t, cost.InputCost)
		})
	}
}

func TestDeepSeekMultiTurnBillingUsesIndividualContexts(t *testing.T) {
	svc := &OpenAIGatewayService{billingService: newTestBillingService()}
	key := &APIKey{}
	turns := []OpenAIForwardResult{{Usage: OpenAIUsage{InputTokens: 150000, OutputTokens: 50}}, {Usage: OpenAIUsage{InputTokens: 150000, OutputTokens: 50}}}
	tools := &DeepSeekToolUsage{SearchCount: 1, FetchCount: 1, SearchPrice: .006, FetchPrice: .006}
	result := &OpenAIForwardResult{DeepSeekTools: tools, DeepSeekModelTurns: turns}
	total, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, key, []string{"claude-sonnet-4"}, .35, 1, 1, 1, UsageTokens{}, "", nil, false, time.Now())
	require.NoError(t, err)
	single, err := svc.calculateOpenAIRecordUsageCost(context.Background(), &turns[0], key, []string{"claude-sonnet-4"}, .35, 1, 1, 1, UsageTokens{InputTokens: 150000, OutputTokens: 50}, "", nil, false, time.Now())
	require.NoError(t, err)
	require.InDelta(t, 2*single.ActualCost+.012, total.ActualCost, 1e-12)
}

type deepSeekAccountFixture struct {
	AccountRepository
	account *Account
}

func (r deepSeekAccountFixture) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

type deepSeekHTTPFixture struct {
	HTTPUpstream
	fn func(*http.Request) (*http.Response, error)
}

type deepSeekJournalFixture struct {
	UsageBillingRepository
	checkpoints []*DeepSeekUsageCheckpoint
}

func (j *deepSeekJournalFixture) BeginDeepSeekUsage(context.Context, string, int64, int64, int64, time.Time) error {
	return nil
}
func (j *deepSeekJournalFixture) SaveDeepSeekUsage(_ context.Context, value *DeepSeekUsageCheckpoint) error {
	wire, _ := json.Marshal(value)
	var frozen DeepSeekUsageCheckpoint
	_ = json.Unmarshal(wire, &frozen)
	j.checkpoints = append(j.checkpoints, &frozen)
	return nil
}
func (j *deepSeekJournalFixture) SettleDeepSeekUsage(context.Context, string, int64) (*DeepSeekUsageSettlement, error) {
	return &DeepSeekUsageSettlement{Result: &UsageBillingApplyResult{}}, nil
}
func (j *deepSeekJournalFixture) ListPendingDeepSeekUsage(context.Context, int) ([]DeepSeekUsageIdentity, error) {
	return nil, nil
}

func (h deepSeekHTTPFixture) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.fn(r)
}
func deepSeekFixtureResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestDeepSeekHostedMoreThanThreeRounds(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				price := 6.0
				groupID := int64(1)
				key := &APIKey{ID: 4, UserID: 2, GroupID: &groupID, User: &User{ID: 2, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, FetchPricePer1K: &price}}}
				account := &Account{ID: 18, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example/v1", "api_protocol": "chat_completions"}, Extra: map[string]any{"deepseek_tools_enabled": true, "deepseek_search_cost_per_call": .005, "deepseek_fetch_cost_per_call": .005}}
				models, tools := 0, 0
				httpStub := deepSeekHTTPFixture{fn: func(req *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					if strings.HasSuffix(req.URL.Path, "/tools/search") {
						tools++
						return deepSeekFixtureResponse(`{"results":[{"title":"A source","url":"https://example.com","snippet":"Evidence"}]}`), nil
					}
					require.True(t, strings.HasSuffix(req.URL.Path, "/chat/completions"))
					models++
					content := `"content":"Final answer https://example.com"`
					finish := "stop"
					if models <= 4 {
						var name string
						for _, tool := range gjson.GetBytes(body, "tools").Array() {
							n := tool.Get("function.name").String()
							if strings.Contains(n, "sources_web_search_") {
								name = n
							}
						}
						require.NotEmpty(t, name)
						content = fmt.Sprintf(`"content":null,"reasoning_content":"thinking", "tool_calls":[{"id":"call_%d","type":"function","function":{"name":%q,"arguments":"{\"query\":\"Python\"}"}}]`, models, name)
						finish = "tool_calls"
					}
					return deepSeekFixtureModelResponse(t, stream, fmt.Sprintf(`{"id":"chatcmpl-%d","object":"chat.completion","created":123,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant",%s},"finish_reason":%q}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`, models, content, finish)), nil
				}}
				journal := &deepSeekJournalFixture{}
				cfg := deepSeekChatFallbackTestConfig()
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: httpStub, accountRepo: deepSeekAccountFixture{account: account}, billingService: newTestBillingService(), usageBillingRepo: journal, billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
				body := map[string]any{"model": "deepseek-flash", "stream": stream}
				switch protocol {
				case "responses":
					body["input"] = "Find Python"
				default:
					body["messages"] = []any{map[string]any{"role": "user", "content": "Find Python"}}
				}
				if protocol == "messages" {
					body["max_tokens"] = 128
				}
				wire, _ := json.Marshal(body)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest("POST", "/v1/test", strings.NewReader(string(wire)))
				c.Set("api_key", key)
				SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})
				result, handled, err := svc.tryForwardDeepSeekTools(context.Background(), c, account, wire, protocol, "")
				require.True(t, handled)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 5, models)
				require.Equal(t, 4, tools)
				require.Equal(t, 4, result.DeepSeekTools.SearchCount)
				require.Equal(t, 500, result.Usage.InputTokens)
				require.Len(t, journal.checkpoints, 9)
				require.InDelta(t, .024, journal.checkpoints[8].Usage.BillingSnapshot.DeepSeekTools.Cost(), 1e-12)
				require.Contains(t, recorder.Body.String(), "https://example.com")
				if stream {
					require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
					require.NotContains(t, recorder.Body.String(), "sources_web_")
					terminal := "data: [DONE]"
					if protocol == "responses" {
						terminal = "event: response.completed"
					}
					if protocol == "messages" {
						terminal = "event: message_stop"
						require.Contains(t, recorder.Body.String(), `"tool_search":4`)
					}
					require.Equal(t, 1, strings.Count(recorder.Body.String(), terminal))
				}
			})
		}
	}
}

// Model a provider that honors stream. Legacy JSON-only fixtures could not
// detect hosted buffering, even when their outer test requested SSE.
func deepSeekFixtureModelResponse(t *testing.T, stream bool, body string) *http.Response {
	t.Helper()
	if !stream {
		return deepSeekFixtureResponse(body)
	}
	var root map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &root))
	choices := root["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if calls, ok := message["tool_calls"].([]any); ok {
		for i, call := range calls {
			call.(map[string]any)["index"] = i
		}
	}
	chunk := func(delta map[string]any, finish any, usage any) string {
		payload := map[string]any{"id": root["id"], "object": "chat.completion.chunk", "created": 123, "model": root["model"], "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			payload["usage"] = usage
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		return "data: " + string(raw) + "\n\n"
	}
	frames := chunk(message, nil, nil) + chunk(map[string]any{}, choice["finish_reason"], root["usage"]) + "data: [DONE]\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}
}
