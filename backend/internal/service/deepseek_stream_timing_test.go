//go:build unit

package service

// Regression: hosted auto-tools must preserve upstream streaming and flush
// visible content before the delayed upstream terminal.
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type timingProbeBody struct {
	first, tail  *strings.Reader
	delay        bool
	terminalRead *bool
}

func (b *timingProbeBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	if !b.delay {
		b.delay = true
		time.Sleep(180 * time.Millisecond)
		*b.terminalRead = true
	}
	return b.tail.Read(p)
}

type timingProbeWriter struct {
	*httptest.ResponseRecorder
	terminalRead          *bool
	visible               bool
	visibleBeforeTerminal bool
}

func (w *timingProbeWriter) Flush() {
	if !w.visible && strings.Contains(w.Body.String(), "diagnostic-first-token") {
		w.visible = true
		w.visibleBeforeTerminal = !*w.terminalRead
	}
	w.ResponseRecorder.Flush()
}

func TestDeepSeekStreamingTimingContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, mode := range []string{"auto_stream", "tool_choice_none_stream", "auto_nonstream"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				stream := mode != "auto_nonstream"
				price, groupID := 6.0, int64(218)
				key := &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: 10}, Group: &Group{ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: .35, SearchPricePer1k: &price, DeepSeekTools: DeepSeekToolsConfig{Enabled: true, AutoInject: true, FetchPricePer1K: &price}}}
				account := &Account{ID: 4, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://provider.example/v1", "api_protocol": "chat_completions"}, Extra: map[string]any{"deepseek_tools_enabled": true, "deepseek_search_cost_per_call": .005, "deepseek_fetch_cost_per_call": .005}}
				terminalRead, upstreamStream := false, false
				modelCalls := 0
				upstream := deepSeekHTTPFixture{fn: func(r *http.Request) (*http.Response, error) {
					require.True(t, strings.HasSuffix(r.URL.Path, "/chat/completions"), "no external tool/model request is permitted")
					modelCalls++
					raw, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					upstreamStream = gjson.GetBytes(raw, "stream").Bool()
					first, tail, ctype := "", `{"id":"chat-probe","object":"chat.completion","created":123,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"diagnostic-first-token last"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`, "application/json"
					if upstreamStream {
						ctype = "text/event-stream"
						first = "data: " + `{"id":"chat-probe","object":"chat.completion.chunk","created":123,"model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"diagnostic-first-token"},"finish_reason":null}]}` + "\n\n"
						tail = "data: " + `{"id":"chat-probe","object":"chat.completion.chunk","created":123,"model":"deepseek-flash","choices":[{"index":0,"delta":{"content":" last"},"finish_reason":null}]}` + "\n\ndata: " + `{"id":"chat-probe","object":"chat.completion.chunk","created":123,"model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}` + "\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ctype}}, Body: io.NopCloser(&timingProbeBody{first: strings.NewReader(first), tail: strings.NewReader(tail), terminalRead: &terminalRead})}, nil
				}}
				cfg := deepSeekChatFallbackTestConfig()
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, accountRepo: deepSeekAccountFixture{account: account}, usageBillingRepo: &deepSeekJournalFixture{}, billingService: newTestBillingService(), billingCacheService: &BillingCacheService{cfg: cfg, cache: newMemInflightCache(10)}}
				body := map[string]any{"model": "deepseek-flash", "stream": stream}
				if protocol == "responses" {
					body["input"] = "Say hello"
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "Say hello"}}
				}
				if protocol == "messages" {
					body["max_tokens"] = 128
				}
				if mode == "tool_choice_none_stream" {
					if protocol == "messages" {
						body["tool_choice"] = map[string]any{"type": "none"}
					} else {
						body["tool_choice"] = "none"
					}
				}
				wire, err := json.Marshal(body)
				require.NoError(t, err)
				writer := &timingProbeWriter{ResponseRecorder: httptest.NewRecorder(), terminalRead: &terminalRead}
				c, _ := gin.CreateTestContext(writer)
				c.Request = httptest.NewRequest("POST", "/v1/test", strings.NewReader(string(wire)))
				c.Set("api_key", key)
				SetDeepSeekBillingInput(c, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, PricingAt: time.Now()})
				var result *OpenAIForwardResult
				switch protocol {
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, wire, "", "")
				case "messages":
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, wire, "", "")
				default:
					result, err = svc.Forward(context.Background(), c, account, wire)
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 1, modelCalls)
				require.Contains(t, writer.Body.String(), "diagnostic-first-token")
				first := any(nil)
				if result.FirstTokenMs != nil {
					first = *result.FirstTokenMs
				}
				t.Logf("client_stream=%v upstream_stream=%v visible_before_terminal=%v first_token_ms=%v duration_ms=%d", stream, upstreamStream, writer.visibleBeforeTerminal, first, result.Duration.Milliseconds())
				assert.Equal(t, stream, upstreamStream, "client stream contract must survive hosted tool injection even when no tool is called")
				if stream {
					assert.True(t, writer.visibleBeforeTerminal, "first content must be flushed before upstream generation completes")
					require.NotNil(t, result.FirstTokenMs)
					assert.GreaterOrEqual(t, result.Duration.Milliseconds()-int64(*result.FirstTokenMs), int64(100), "injected 180 ms generation tail must not be charged to TTFT")
				} else {
					assert.Nil(t, result.FirstTokenMs, "a buffered non-stream reply must not invent a first-token measurement")
				}
			})
		}
	}
}
