//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

func TestDeepSeekHostedRequestTimeoutDoesNotSignalAccountFailover(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			result, _, _, err := deepSeekHostedProtocolFixture(t, protocol, map[string]any{}, func(*http.Request) (*http.Response, error) {
				calls++
				response := deepSeekFixtureResponse(`{"error":{"message":"fixture timeout"}}`)
				response.StatusCode = http.StatusRequestTimeout
				return response, nil
			})
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Equal(t, 1, calls)
			require.Nil(t, result, "there is no confirmed work to settle")
		})
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
