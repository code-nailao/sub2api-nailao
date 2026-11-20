//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

func TestDeepSeekChatJSONCancellationDuringScan(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"large_string", `"` + strings.Repeat("x", 8<<20) + `"`},
		{"many_tokens", `[` + strings.Repeat(`null,`, 1<<16) + `null]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &deepSeekCancelDuringScan{Context: ctx, cancel: cancel, after: 16}
			body := []byte(`{"metadata":` + tc.value + `}`)
			started := time.Now()
			err := validateDeepSeekChatJSON(probe, body)
			require.Less(t, time.Since(started), 2*time.Second)
			require.ErrorIs(t, err, context.Canceled)
			require.GreaterOrEqual(t, probe.checks, probe.after)
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
				body := []byte(`{"metadata":` + strings.Repeat(open, depth-1) + `null` + strings.Repeat(close, depth-1) + `}`)
				err := validateDeepSeekChatJSON(context.Background(), body)
				if depth == deepSeekChatJSONMaxDepth {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "nesting")
				}
			})
		}
	}
}

func TestDeepSeekChatJSONKeepsObjectScopesAndDecodedKeys(t *testing.T) {
	for _, tc := range []struct{ name, body, failure string }{
		{"siblings", `{"metadata":[{"name":"lookup"},{"name":"lookup"}],"seed":123456789012345678901234567890}`, ""},
		{"nested_scope", `{"metadata":{"nested":{"name":"lookup"},"name":"lookup","empty":{}},"array":[[],{},true,false,null]}`, ""},
		{"escaped_duplicate", `{"metadata":[{"\u006eame":"lookup","name":"lookup"}]}`, "duplicate"},
		{"escaped_quotes", `{"metadata":{"a\"b":null,"a\u0022b":null}}`, "duplicate"},
		{"trailing_document", `{"metadata":null}{}`, "invalid"},
		{"unfinished_object", `{"metadata":`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeepSeekChatJSON(context.Background(), []byte(tc.body))
			if tc.failure == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.failure)
			}
		})
	}
}

func TestDeepSeekReviewUnknownTransportMustNotReplayWithoutTools(t *testing.T) {
	calls := 0
	_, _, _, err := deepSeekHostedTestFixture(t, map[string]any{}, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return deepSeekFixtureResponse(deepSeekModelFixtureBody(t, r, "", "")), nil
	})
	require.Error(t, err, "unknown transport outcome must not become a successful replay")
	require.Equal(t, 1, calls, "no second model request after an unknown transport outcome")
}

func TestDeepSeekReviewDisableHostedToolsPreservesForcedClientChoice(t *testing.T) {
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"Call the client tool"}],"tools":[{"type":"function","function":{"name":"client_search","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"client_search"}}}`)
	req, _, err := prepareDeepSeekHosted(body, "chat", true)
	require.NoError(t, err)
	choice := req.Body["tool_choice"]
	req.disableServerTools()
	require.Equal(t, choice, req.Body["tool_choice"], "disabling hosted tools must not relax an unrelated forced client function")
	require.Len(t, req.Body["tools"], 1)
}

func TestDeepSeekReviewHTTPFallbackBoundaries(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, status := range []int{401, 403, 429, 500, 502, 503, 504} {
				t.Run(fmt.Sprintf("%s/stream=%v/http=%d", protocol, stream, status), func(t *testing.T) {
					calls := 0
					_, _, _, err := deepSeekHostedProtocolFixture(t, protocol, map[string]any{"stream": stream}, func(r *http.Request) (*http.Response, error) {
						calls++
						response := deepSeekFixtureResponse(`{"error":{"message":"fixture failure"}}`)
						response.StatusCode = status
						return response, nil
					})
					require.Error(t, err)
					expected := 1
					if status == 500 || status == 502 || status == 503 {
						expected = 2
					}
					require.Equal(t, expected, calls, "fallback is bounded; auth/rate-limit/timeout must not replay")
				})
			}
		}
	}
}

func TestDeepSeekReviewFailedBatchStopsRemainingServerCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			models, tools := 0, 0
			result, _, _, err := deepSeekHostedTestFixture(t, map[string]any{"stream": stream}, func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/tools/") {
					tools++
					return nil, io.ErrUnexpectedEOF
				}
				models++
				if models == 1 {
					body := deepSeekModelFixtureBody(t, r, "call_1", "query")
					second := gjson.Get(body, "choices.0.message.tool_calls.0").Value().(map[string]any)
					second["id"] = "call_2"
					var e error
					body, e = sjson.Set(body, "choices.0.message.tool_calls.-1", second)
					require.NoError(t, e)
					return deepSeekFixtureModelResponse(t, stream, body), nil
				}
				raw, e := io.ReadAll(r.Body)
				require.NoError(t, e)
				require.Empty(t, gjson.GetBytes(raw, "tools").Array())
				var toolIDs []string
				for _, m := range gjson.GetBytes(raw, "messages").Array() {
					if m.Get("role").String() == "tool" {
						toolIDs = append(toolIDs, m.Get("tool_call_id").String())
						require.Contains(t, m.Get("content").String(), "unavailable")
					}
				}
				require.ElementsMatch(t, []string{"call_1", "call_2"}, toolIDs)
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
				return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, r, "", "")), nil
			})
			require.NoError(t, err)
			require.Equal(t, 2, models)
			require.Equal(t, 1, tools)
			require.Zero(t, result.DeepSeekTools.Cost())
		})
	}
}

func TestDeepSeekReviewConfirmedWorkNeverReplaysModelHTTPFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			models, tools := 0, 0
			result, _, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": stream}, func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/tools/") {
					tools++
					return deepSeekFixtureResponse(`{"results":[{"url":"https://example.com"}]}`), nil
				}
				models++
				if models == 1 {
					return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, r, "call_1", "query")), nil
				}
				res := deepSeekFixtureResponse(`{"error":{"message":"fixture failure"}}`)
				res.StatusCode = 502
				return res, nil
			})
			require.Error(t, err)
			require.Equal(t, 2, models)
			require.Equal(t, 1, tools)
			require.Equal(t, 1, result.DeepSeekTools.SearchCount)
			if stream {
				require.Contains(t, response, "confirmed usage are retained")
				require.NotContains(t, response, "[DONE]")
			}
		})
	}
}

func TestDeepSeekReviewUnknownTransportDoesNotReuseOldHTTPDiagnostic(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			var captured *gin.Context
			_, _, _, err := deepSeekHostedProtocolFixture(t, protocol, map[string]any{}, func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, io.ErrUnexpectedEOF
			}, func(c *gin.Context) {
				captured = c
				c.Set(OpsUpstreamErrorsKey, []*OpsUpstreamErrorEvent{{Kind: "failover", UpstreamStatusCode: 502}})
			})
			require.Error(t, err)
			require.Equal(t, 1, calls)
			events, ok := captured.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			list := events.([]*OpsUpstreamErrorEvent)
			require.Len(t, list, 2, "new native transport diagnostic must reach parent context")
			require.Equal(t, "request_error", list[1].Kind)
			require.Zero(t, list[1].UpstreamStatusCode)
		})
	}
}

func TestDeepSeekReviewPartialStreamFailureMarksOpsAndNeverReplays(t *testing.T) {
	calls := 0
	var captured *gin.Context
	result, _, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true}, func(r *http.Request) (*http.Response, error) {
		calls++
		wire := `data: {"id":"partial","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"partial answer"},"finish_reason":null}]}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
	}, func(c *gin.Context) { captured = c })
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, calls)
	require.Contains(t, response, "partial answer")
	require.NotContains(t, response, "[DONE]")
	_, ok := GetOpsStreamError(captured)
	require.True(t, ok, "in-band failure must be visible to ops despite HTTP200")
}
