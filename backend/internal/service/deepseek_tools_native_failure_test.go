//go:build unit

package service

import (
	"context"
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

func TestDeepSeekNativeFailedStreamPreservesConfirmedUsage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		confirmed   bool
		eventHeader bool
	}{
		{name: "confirmed_terminal_usage", confirmed: true},
		{name: "no_terminal_usage"},
		{name: "confirmed_terminal_usage_event_header", confirmed: true, eventHeader: true},
		{name: "no_terminal_usage_event_header", eventHeader: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			price := 6.0
			groupID := int64(218)
			key := &APIKey{ID: 4, UserID: 2, GroupID: &groupID, User: &User{ID: 2, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, FetchPricePer1K: &price}}}
			account := &Account{ID: 18, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://provider.example/v1", "api_protocol": "responses"}, Extra: map[string]any{"deepseek_tools_enabled": true, "deepseek_native_tools_enabled": true, "deepseek_search_cost_per_call": .005, "deepseek_fetch_cost_per_call": .005}}
			usage := `,"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":20},"server_tool_use":{"tool_search":1,"tool_fetch":2}}`
			terminalUsage := ""
			if tc.confirmed {
				terminalUsage = usage
			}
			terminalPrefix := `data: {"type":"response.failed",`
			if tc.eventHeader {
				terminalPrefix = "event: response.failed\ndata: {"
			}
			stream := "data: " + `{"type":"response.in_progress","response":{"id":"resp_native_failed","status":"in_progress"` + usage + "}}\n\n" +
				"data: " + `{"type":"response.output_text.delta","response_id":"resp_native_failed","output_index":0,"content_index":0,"delta":"Partial answer"}` + "\n\n" +
				terminalPrefix + `"response":{"id":"resp_native_failed","model":"deepseek-pro","status":"failed","error":{"code":"server_error","message":"continuation failed"}` + terminalUsage + "}}\n\n"
			requests := 0
			httpStub := deepSeekHTTPFixture{fn: func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "/v1/responses", req.URL.Path)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			}}
			journal := &deepSeekJournalFixture{}
			cfg := deepSeekChatFallbackTestConfig()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: httpStub, accountRepo: deepSeekAccountFixture{account: account}, billingService: newTestBillingService(), usageBillingRepo: journal, billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
			body := []byte(`{"model":"deepseek-pro","stream":true,"input":"Find a source"}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			c.Set("api_key", key)
			SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})

			result, _, handled, err := svc.tryForwardDeepSeekTools(context.Background(), c, account, body, "responses", "")
			require.True(t, handled)
			require.ErrorContains(t, err, "upstream response failed")
			require.Equal(t, 1, requests)
			require.Contains(t, recorder.Body.String(), "Partial answer")
			require.Contains(t, recorder.Body.String(), "response.failed")
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if data, ok := extractOpenAISSEDataLine(line); ok && gjson.Get(data, "response.status").String() == "failed" {
					require.False(t, gjson.Get(data, "response.usage").Exists(), "client failure terminal must stay sanitized")
				}
			}
			if !tc.confirmed {
				require.Nil(t, result)
				require.Empty(t, journal.checkpoints)
				return
			}
			require.NotNil(t, result)
			require.Equal(t, OpenAIUsage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 20}, result.Usage)
			require.Equal(t, "deepseek-pro", result.Model)
			require.Equal(t, "deepseek-pro", result.BillingModel)
			require.Equal(t, 1, result.DeepSeekTools.SearchCount)
			require.Equal(t, 2, result.DeepSeekTools.FetchCount)
			require.Len(t, journal.checkpoints, 1)
			checkpoint := journal.checkpoints[0]
			require.Equal(t, result.RequestID, checkpoint.Usage.RequestID)
			require.Equal(t, result.RequestID, checkpoint.Command.RequestID)
			require.Equal(t, 80, checkpoint.Usage.InputTokens)
			require.Equal(t, 10, checkpoint.Usage.OutputTokens)
			require.Equal(t, 20, checkpoint.Usage.CacheReadTokens)
			require.InDelta(t, .018, checkpoint.Usage.BillingSnapshot.DeepSeekTools.Cost(), 1e-12)
			require.False(t, checkpoint.Ready)
		})
	}
}
