//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func deepSeekHostedTestFixture(t *testing.T, body map[string]any, upstream func(*http.Request) (*http.Response, error), configure ...func(*gin.Context)) (*OpenAIForwardResult, *deepSeekJournalFixture, string, error) {
	t.Helper()
	return deepSeekHostedProtocolFixture(t, "chat", body, upstream, configure...)
}

func deepSeekHostedProtocolFixture(t *testing.T, protocol string, body map[string]any, upstream func(*http.Request) (*http.Response, error), configure ...func(*gin.Context)) (*OpenAIForwardResult, *deepSeekJournalFixture, string, error) {
	t.Helper()
	body["model"] = "deepseek-flash"
	if protocol == "responses" {
		body["input"] = "Use the public web"
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "Use the public web"}}
	}
	if protocol == "messages" {
		if _, ok := body["max_tokens"]; !ok {
			body["max_tokens"] = 128
		}
	}
	wire, _ := json.Marshal(body)
	return deepSeekHostedWireFixture(t, deepSeekChatFallbackTestConfig(), protocol, wire, upstream, configure...)
}

func deepSeekHostedWireFixture(t *testing.T, cfg *config.Config, protocol string, wire []byte, upstream func(*http.Request) (*http.Response, error), configure ...func(*gin.Context)) (*OpenAIForwardResult, *deepSeekJournalFixture, string, error) {
	t.Helper()
	price, groupID := 6.0, int64(218)
	key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: .35, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, FetchPricePer1K: &price}}}
	account := &Account{ID: 4, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://provider.example/v1", "api_protocol": "chat_completions"}, Extra: map[string]any{"deepseek_tools_enabled": true, "deepseek_search_cost_per_call": .005, "deepseek_fetch_cost_per_call": .005}}
	journal := &deepSeekJournalFixture{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: deepSeekHTTPFixture{fn: upstream}, accountRepo: deepSeekAccountFixture{account: account}, usageBillingRepo: journal, billingService: newTestBillingService(), billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(wire)))
	c.Set("api_key", key)
	SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})
	for _, setup := range configure {
		setup(c)
	}
	result, _, handled, err := svc.tryForwardDeepSeekTools(c.Request.Context(), c, account, wire, protocol, "")
	require.True(t, handled)
	return result, journal, recorder.Body.String(), err
}

func deepSeekModelFixtureBody(t *testing.T, r *http.Request, callID, query string) string {
	t.Helper()
	wire, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	name := ""
	for _, tool := range gjson.GetBytes(wire, "tools").Array() {
		if n := tool.Get("function.name").String(); strings.Contains(n, "sources_web_search_") {
			name = n
		}
	}
	message := map[string]any{"role": "assistant", "content": "Final answer"}
	finish := "stop"
	if callID != "" {
		require.NotEmpty(t, name)
		args, _ := json.Marshal(map[string]string{"query": query})
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}}}
		finish = "tool_calls"
	}
	response := map[string]any{"id": "chat-fixture", "object": "chat.completion", "model": "deepseek-flash", "created": 123, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}}
	out, _ := json.Marshal(response)
	return string(out)
}

func TestDeepSeekConfirmedToolSurvivesLaterModelFailure(t *testing.T) {
	models, tools := 0, 0
	result, journal, _, err := deepSeekHostedTestFixture(t, map[string]any{}, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/tools/search") {
			tools++
			require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
			return deepSeekFixtureResponse(`{"results":[{"url":"https://example.com","title":"Source"}]}`), nil
		}
		models++
		if models == 1 {
			return deepSeekFixtureResponse(deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
		}
		return nil, errors.New("fixture model continuation failed")
	})
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, tools)
	require.Equal(t, 1, result.DeepSeekTools.SearchCount)
	require.Greater(t, len(journal.checkpoints), 0)
	last := journal.checkpoints[len(journal.checkpoints)-1]
	require.InDelta(t, .006, last.Usage.BillingSnapshot.DeepSeekTools.Cost(), 1e-12)
	require.Greater(t, last.Command.BalanceCost, .006, "confirmed model tokens remain payable too")
}

func TestDeepSeekEmptyAndFailedToolsNeverAddFees(t *testing.T) {
	for _, response := range []string{`{"results":[]}`, `{"error":{"message":"unavailable"}}`, `{"results":[null]}`} {
		t.Run(response, func(t *testing.T) {
			models, tools := 0, 0
			result, journal, _, err := deepSeekHostedTestFixture(t, map[string]any{}, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/tools/search") {
					tools++
					return deepSeekFixtureResponse(response), nil
				}
				models++
				id := ""
				if models == 1 {
					id = "call_1"
				}
				return deepSeekFixtureResponse(deepSeekModelFixtureBody(t, r, id, "Python")), nil
			})
			require.NoError(t, err)
			require.Equal(t, 1, tools)
			require.Zero(t, result.DeepSeekTools.Cost())
			require.Equal(t, 2, models)
			require.Len(t, journal.checkpoints, 2)
		})
	}
}

func TestDeepSeekCallIDDedupAndConflicts(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			models, tools := 0, 0
			result, _, _, err := deepSeekHostedTestFixture(t, map[string]any{}, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/tools/search") {
					tools++
					return deepSeekFixtureResponse(`{"results":[{"url":"https://example.com"}]}`), nil
				}
				models++
				id, query := "", "Python"
				if models <= 2 {
					id = "same_call"
				}
				if models == 2 && changed {
					query = "different"
				}
				return deepSeekFixtureResponse(deepSeekModelFixtureBody(t, r, id, query)), nil
			})
			if changed {
				require.ErrorContains(t, err, "different arguments")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, tools)
			require.Equal(t, 1, result.DeepSeekTools.SearchCount)
		})
	}
}

func TestDeepSeekTotalOutputBudgetStopsBeforeTool(t *testing.T) {
	tools := 0
	result, _, _, err := deepSeekHostedTestFixture(t, map[string]any{"max_completion_tokens": 5}, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/tools/") {
			tools++
			return nil, errors.New("must not execute")
		}
		return deepSeekFixtureResponse(deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
	})
	require.ErrorContains(t, err, "output token budget")
	require.Zero(t, tools)
	require.Equal(t, 10, result.Usage.OutputTokens)
}

func TestDeepSeekLimitsKeepClientFunctions(t *testing.T) {
	req, handled, err := prepareDeepSeekHosted([]byte(`{"max_tool_calls":1,"tools":[{"type":"function","function":{"name":"client_search","parameters":{"type":"object"}}}]}`), "chat", true)
	require.NoError(t, err)
	require.True(t, handled)
	req.Calls = 1
	req.removeExhaustedTools()
	wire, _ := json.Marshal(req.Body)
	require.Contains(t, string(wire), "client_search")
	require.NotContains(t, string(wire), "sources_web_")
	require.NotEqual(t, "none", req.Body["tool_choice"])
}
