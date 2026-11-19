//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const rejectionGatewayTestModel = "grok-4.7"
const rejectionGatewayTestText = "I'm sorry, I cannot help with that request."

// These tests exercise the real forwarding/parsing seam with a fake HTTP
// transport. No external model, billing account, or production data is used.
func rejectionGatewayTestAccount() *Account {
	a := rawChatCompletionsTestAccount()
	a.Platform = PlatformGrok
	a.Name = "supplier-refusal-fixture"
	a.Credentials["pool_mode"] = true
	a.Credentials["pool_mode_retry_count"] = 10
	a.Credentials["pool_mode_retry_status_codes"] = []any{400, 403}
	a.Credentials["model_mapping"] = map[string]any{rejectionGatewayTestModel: rejectionGatewayTestModel}
	a.Extra = map[string]any{
		"upstream_rejection_profile":  "grok_content_rejection_v1",
		"upstream_rejection_cost":     0.1,
		"upstream_rejection_currency": "USD",
	}
	return a
}

func rejectionGatewayContext(path string, request []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(request))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func rejectionGatewayResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{contentType},
			"X-Request-Id": []string{"supplier-refusal-fixture"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func rejectionGatewayService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	return &OpenAIGatewayService{
		cfg:                  cfg,
		httpUpstream:         upstream,
		toolCorrector:        NewCodexToolCorrector(),
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
	}
}

func requireNoRejectionFailover(t *testing.T, err error) {
	t.Helper()
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "a known supplier charge must never be replayed")
}

func TestUpstreamRejectionGatewayHTTPRefusalPreservesOutcomeBeforeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"image_400", http.StatusBadRequest, `{"error":{"message":"Generated image rejected by content moderation"}}`},
		{"video_400", http.StatusBadRequest, `{"error":{"message":"Generated video rejected by content moderation"}}`},
		{"policy_403", http.StatusForbidden, `{"error":{"message":"This content violates usage guidelines"}}`},
	} {
		for _, protocol := range []string{"chat", "responses", "passthrough", "messages"} {
			t.Run(tc.name+"/"+protocol, func(t *testing.T) {
				account := rejectionGatewayTestAccount()
				upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(tc.status, "application/json", tc.body)}
				svc := rejectionGatewayService(upstream)
				var result *OpenAIForwardResult
				var err error
				var rec *httptest.ResponseRecorder
				switch protocol {
				case "chat":
					body := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"harmless fixture"}],"stream":false}`)
					c, w := rejectionGatewayContext("/v1/chat/completions", body)
					rec = w
					result, err = svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
				case "responses", "passthrough":
					body := []byte(`{"model":"grok-4.7","input":"harmless fixture","stream":false}`)
					c, w := rejectionGatewayContext("/v1/responses", body)
					rec = w
					if protocol == "responses" {
						result, err = svc.forwardGrokResponses(context.Background(), c, account, body, rejectionGatewayTestModel, false, time.Now())
					} else {
						result, err = svc.forwardOpenAIPassthrough(context.Background(), c, account, body, nil, rejectionGatewayTestModel, false, nil, false, time.Now())
					}
				case "messages":
					body := []byte(`{"model":"grok-4.7","max_tokens":16,"messages":[{"role":"user","content":"harmless fixture"}],"stream":false}`)
					c, w := rejectionGatewayContext("/v1/messages", body)
					rec = w
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				}
				require.Error(t, err)
				var refused *UpstreamRejectionError
				require.ErrorAs(t, err, &refused)
				requireNoRejectionFailover(t, err)
				require.NotNil(t, result)
				require.NotNil(t, result.UpstreamRejection)
				require.Equal(t, tc.status, result.UpstreamRejection.HTTPStatus)
				require.Equal(t, tc.status, rec.Code)
				require.Len(t, upstream.requests, 1, "pool mode must not turn a charged rejection into more supplier calls")
				require.Equal(t, "supplier-refusal-fixture", result.RequestID)
				require.Zero(t, result.Usage.InputTokens)
				require.Zero(t, result.Usage.OutputTokens)
				require.False(t, result.SucceededForScheduling())
			})
		}
	}
}

func TestUpstreamRejectionGatewayChatZeroUsageIsTerminalOnlyForConfiguredRefusal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		text       string
		configured bool
		stream     bool
		wantFee    bool
	}{
		{"refusal", rejectionGatewayTestText, true, false, true},
		{"stream_requested_json_refusal", rejectionGatewayTestText, true, true, true},
		{"normal_content", "Hello", true, false, false},
		{"profile_not_enabled", rejectionGatewayTestText, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := rejectionGatewayTestAccount()
			if !tc.configured {
				delete(account.Extra, "upstream_rejection_profile")
			}
			payload, err := json.Marshal(map[string]any{
				"id": "chatcmpl_fixture", "object": "chat.completion", "model": rejectionGatewayTestModel,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": tc.text}, "finish_reason": "stop"}},
			})
			require.NoError(t, err)
			request, err := json.Marshal(map[string]any{"model": rejectionGatewayTestModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": tc.stream})
			require.NoError(t, err)
			c, rec := rejectionGatewayContext("/v1/chat/completions", request)
			upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "application/json", string(payload))}
			result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, account, request, "")
			if tc.wantFee {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, result.UpstreamRejection)
				require.Equal(t, "chat.completed", result.UpstreamTerminalEvent)
				require.Zero(t, result.Usage.InputTokens)
				require.Zero(t, result.Usage.OutputTokens)
				require.Contains(t, rec.Body.String(), tc.text)
			} else {
				require.Nil(t, result)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.False(t, c.Writer.Written())
			}
		})
	}
}

type rejectionGatewayBrokenReader struct{}

func (rejectionGatewayBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestUpstreamRejectionGatewayChatStreamKeepsChargeAcrossDeltasAndDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prefix := strings.Join([]string{
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"I'm sorry, I can"}}]}`,
		"",
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"content":"not help with "}}]}`,
		"",
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"content":"that request."}}]}`,
		"", "",
	}, "\n")
	for _, terminal := range []string{"done", "clean_eof", "broken_connection"} {
		t.Run(terminal, func(t *testing.T) {
			payload := prefix
			if terminal == "done" {
				payload += "data: {\"model\":\"grok-4.7\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			}
			resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)
			if terminal == "broken_connection" {
				resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(payload), rejectionGatewayBrokenReader{}))
			}
			request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
			c, _ := rejectionGatewayContext("/v1/chat/completions", request)
			upstream := &httpUpstreamRecorder{resp: resp}
			result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, rejectionGatewayTestAccount(), request, "")
			require.NotNil(t, result)
			require.NotNil(t, result.UpstreamRejection)
			require.Equal(t, http.StatusOK, result.UpstreamRejection.HTTPStatus)
			require.Zero(t, result.Usage.OutputTokens, "fixed charge cannot invent output tokens")
			requireNoRejectionFailover(t, err)
			require.Len(t, upstream.requests, 1)
			if terminal == "done" {
				require.NoError(t, err)
				require.Equal(t, "[DONE]", result.UpstreamTerminalEvent)
			} else {
				var refused *UpstreamRejectionError
				require.ErrorAs(t, err, &refused)
				require.Empty(t, result.UpstreamTerminalEvent, "a charged refusal is evidence without implying a complete generation")
			}
		})
	}
}

func rejectionGatewayResponsesJSON() string {
	return `{"id":"resp_refusal","object":"response","status":"completed","model":"grok-4.7","output":[{"id":"msg_refusal","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"I'm sorry, I cannot help with that request."}]}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}`
}

func rejectionGatewayResponsesSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_refusal","object":"response","model":"grok-4.7","status":"in_progress","output":[]}}`, "",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_refusal","type":"message","role":"assistant","status":"in_progress","content":[]}}`, "",
		`data: {"type":"response.content_part.added","item_id":"msg_refusal","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`, "",
		`data: {"type":"response.output_text.delta","item_id":"msg_refusal","output_index":0,"content_index":0,"delta":"I'm sorry, I cannot "}`, "",
		`data: {"type":"response.output_text.delta","item_id":"msg_refusal","output_index":0,"content_index":0,"delta":"help with that request."}`, "",
		`data: {"type":"response.output_text.done","item_id":"msg_refusal","output_index":0,"content_index":0,"text":"I'm sorry, I cannot help with that request."}`, "",
		`data: {"type":"response.completed","response":` + rejectionGatewayResponsesJSON() + `}`, "",
		`data: {"type":"response.completed","response":` + rejectionGatewayResponsesJSON() + `}`, "",
		"data: [DONE]", "", "",
	}, "\n")
}

func TestUpstreamRejectionGatewayResponsesAndPassthroughKeepOneOutcomeAndRealUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "passthrough"} {
		for _, shape := range []string{"json", "sse", "buffered_sse", "stream_requested_json"} {
			t.Run(protocol+"/"+shape, func(t *testing.T) {
				stream := shape == "sse" || shape == "stream_requested_json"
				payload, contentType := rejectionGatewayResponsesJSON(), "application/json"
				if shape == "sse" || shape == "buffered_sse" {
					payload, contentType = rejectionGatewayResponsesSSE(), "text/event-stream"
				}
				request, err := json.Marshal(map[string]any{"model": rejectionGatewayTestModel, "input": "hello", "stream": stream})
				require.NoError(t, err)
				c, rec := rejectionGatewayContext("/v1/responses", request)
				upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, contentType, payload)}
				svc := rejectionGatewayService(upstream)
				var result *OpenAIForwardResult
				if protocol == "responses" {
					result, err = svc.forwardGrokResponses(context.Background(), c, rejectionGatewayTestAccount(), request, rejectionGatewayTestModel, stream, time.Now())
				} else {
					result, err = svc.forwardOpenAIPassthrough(context.Background(), c, rejectionGatewayTestAccount(), request, nil, rejectionGatewayTestModel, false, nil, stream, time.Now())
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, result.UpstreamRejection)
				require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 4, result.Usage.OutputTokens, "duplicate terminal events must not duplicate usage")
				require.Len(t, upstream.requests, 1)
				require.NotEmpty(t, rec.Body.String())
				require.False(t, result.SucceededForScheduling())
			})
		}
	}
}

func TestUpstreamRejectionGatewayMessagesConversionRetainsOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := rejectionGatewayContext("/v1/messages", nil)
			resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", rejectionGatewayResponsesSSE())
			svc := rejectionGatewayService(nil)
			var result *OpenAIForwardResult
			var err error
			if stream {
				result, err = svc.handleAnthropicStreamingResponse(resp, c, rejectionGatewayTestAccount(), rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
			} else {
				result, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, rejectionGatewayTestAccount(), rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.UpstreamRejection)
			require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			requireNoRejectionFailover(t, err)
			require.Contains(t, rec.Body.String(), "help with that request.")
		})
	}
}

func TestUpstreamRejectionGatewayChatMultilineSSEKeepsWireBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := "data: {\"model\":\"grok-4.7\",\n" +
		"data: \"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"I cannot help with that request.\"}}]}\n\n" +
		"data: {\"model\":\"grok-4.7\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := rejectionGatewayContext("/v1/chat/completions", request)
	upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)}
	result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, rejectionGatewayTestAccount(), request, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.UpstreamRejection)
	require.Equal(t, payload, rec.Body.String(), "classification must not rewrite raw SSE framing")
	require.Len(t, upstream.requests, 1)
}

func TestUpstreamRejectionGatewayMessagesIncompleteStreamKeepsCharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	complete := rejectionGatewayResponsesSSE()
	cut := strings.Index(complete, `data: {"type":"response.completed"`)
	require.Positive(t, cut)
	prefix := complete[:cut]
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := rejectionGatewayContext("/v1/messages", nil)
			resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", prefix)
			svc := rejectionGatewayService(nil)
			var result *OpenAIForwardResult
			var err error
			if stream {
				result, err = svc.handleAnthropicStreamingResponse(resp, c, rejectionGatewayTestAccount(), rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
			} else {
				result, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, rejectionGatewayTestAccount(), rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
			}
			require.Error(t, err)
			var refused *UpstreamRejectionError
			require.ErrorAs(t, err, &refused)
			requireNoRejectionFailover(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.UpstreamRejection)
			require.Zero(t, result.Usage.InputTokens)
			require.Zero(t, result.Usage.OutputTokens)
		})
	}
}

func TestUpstreamRejectionGatewaySSEEventTypeCannotRepairTruncatedEvidence(t *testing.T) {
	for _, fragment := range []string{
		`{"delta":"I cannot help with that request."`,
		`{"delta":"I cannot help with that request.`,
		`{"delta":"I cannot help with that request.","unfinished":`,
	} {
		t.Run(fragment, func(t *testing.T) {
			account := rejectionGatewayTestAccount()
			observer := newUpstreamRejectionSSEObserver(account, http.StatusOK)
			observer.ObserveLine("event: response.output_text.delta")
			observer.ObserveLine("data: " + fragment)
			observer.ObserveLine("")
			observer.Finish()
			require.Nil(t, observer.Evidence(), "event type injection must not turn invalid JSON into a monetary event")
			payload := "event: response.output_text.delta\ndata: " + fragment + "\n\n"
			require.Nil(t, observeUpstreamRejectionBody(account, http.StatusOK, []byte(payload)), "buffered and streaming paths must reject the same malformed evidence")
		})
	}
}

func TestUpstreamRejectionGatewaySSEFrameDoesNotDuplicateDeltas(t *testing.T) {
	observer := newUpstreamRejectionSSEObserver(rejectionGatewayTestAccount(), http.StatusOK)
	observer.ObserveLine(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"I cannot "}}]}`)
	observer.ObserveLine("")
	// A completed frame must not be consumed once as a JSON line and again at
	// its blank line. Duplicating the middle delta would produce "help help"
	// and break the one valid phrase assembled across these three frames.
	require.Nil(t, observer.Evidence())
	observer.ObserveLine(`data: {"choices":[{"index":0,"delta":{"content":"help "}}]}`)
	observer.ObserveLine("")
	require.Nil(t, observer.Evidence())
	observer.ObserveLine(`data: {"choices":[{"index":0,"delta":{"content":"with it"}}]}`)
	observer.ObserveLine("")
	require.NotNil(t, observer.Evidence())
}

func TestUpstreamRejectionGatewayHTTP200SemanticPolicyErrorIsNotHTTP403Fee(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, text := range []string{"content moderation rejected", "I cannot help with that request."} {
		for _, protocol := range []string{"responses", "passthrough"} {
			for _, stream := range []bool{false, true} {
				name := text + "/" + protocol + "/json"
				if stream {
					name = text + "/" + protocol + "/sse"
				}
				t.Run(name, func(t *testing.T) {
					errorJSON, err := json.Marshal(map[string]any{
						"type": "error", "status": 403,
						"error": map[string]any{"code": "content_policy_violation", "message": text},
					})
					require.NoError(t, err)
					payload, contentType := string(errorJSON), "application/json"
					if stream {
						payload, contentType = "data: "+string(errorJSON)+"\n\ndata: [DONE]\n\n", "text/event-stream"
					}
					request, err := json.Marshal(map[string]any{"model": rejectionGatewayTestModel, "input": "hello", "stream": stream})
					require.NoError(t, err)
					c, _ := rejectionGatewayContext("/v1/responses", request)
					upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, contentType, payload)}
					svc := rejectionGatewayService(upstream)
					var result *OpenAIForwardResult
					if protocol == "responses" {
						result, err = svc.forwardGrokResponses(context.Background(), c, rejectionGatewayTestAccount(), request, rejectionGatewayTestModel, stream, time.Now())
					} else {
						result, err = svc.forwardOpenAIPassthrough(context.Background(), c, rejectionGatewayTestAccount(), request, nil, rejectionGatewayTestModel, false, nil, stream, time.Now())
					}
					if result != nil {
						require.Nil(t, result.UpstreamRejection)
					}
					var rejected *UpstreamRejectionError
					require.False(t, errors.As(err, &rejected), "HTTP 200 error fields are not assistant output and cannot be reclassified as HTTP 403")
					require.Len(t, upstream.requests, 1)
				})
			}
		}
	}
}

func TestUpstreamRejectionGatewayResponsesProfileOffPreservesNormalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "passthrough", "messages_buffered", "messages_stream"} {
		t.Run(protocol, func(t *testing.T) {
			account := rejectionGatewayTestAccount()
			delete(account.Extra, "upstream_rejection_profile")
			request := []byte(`{"model":"grok-4.7","input":"hello","stream":false}`)
			c, _ := rejectionGatewayContext("/v1/responses", request)
			upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "application/json", rejectionGatewayResponsesJSON())}
			svc := rejectionGatewayService(upstream)
			var result *OpenAIForwardResult
			var err error
			switch protocol {
			case "responses":
				result, err = svc.forwardGrokResponses(context.Background(), c, account, request, rejectionGatewayTestModel, false, time.Now())
			case "passthrough":
				result, err = svc.forwardOpenAIPassthrough(context.Background(), c, account, request, nil, rejectionGatewayTestModel, false, nil, false, time.Now())
			case "messages_buffered", "messages_stream":
				c, _ = rejectionGatewayContext("/v1/messages", nil)
				resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", rejectionGatewayResponsesSSE())
				if protocol == "messages_buffered" {
					result, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, account, rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
				} else {
					result, err = svc.handleAnthropicStreamingResponse(resp, c, account, rejectionGatewayTestModel, rejectionGatewayTestModel, rejectionGatewayTestModel, time.Now())
				}
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.UpstreamRejection)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.True(t, result.SucceededForScheduling())
		})
	}
}

func TestUpstreamRejectionGatewayProfileDisablesImplicitEncryptedContentReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, configured := range []bool{true, false} {
		name := "profile_on_one_attempt"
		if !configured {
			name = "profile_off_existing_repair_retry"
		}
		t.Run(name, func(t *testing.T) {
			account := rejectionGatewayTestAccount()
			if !configured {
				delete(account.Extra, "upstream_rejection_profile")
			}
			request := []byte(`{
				"model":"grok-4.7","stream":false,
				"input":[
					{"type":"reasoning","summary":[{"type":"summary_text","text":"keep visible summary"}],"encrypted_content":"fixture-opaque-reasoning"},
					{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
				]
			}`)
			c, _ := rejectionGatewayContext("/v1/responses", request)
			c.Set("api_key", &APIKey{ID: 2001})
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				rejectionGatewayResponse(http.StatusBadRequest, "application/json", `{"code":"invalid-argument","error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`),
				rejectionGatewayResponse(http.StatusOK, "application/json", strings.ReplaceAll(rejectionGatewayResponsesJSON(), rejectionGatewayTestText, "OK")),
			}}
			result, err := rejectionGatewayService(upstream).forwardGrokResponses(context.Background(), c, account, request, rejectionGatewayTestModel, false, time.Now())
			if configured {
				require.Error(t, err)
				require.Len(t, upstream.requests, 1, "the admission owner must close the first monetary attempt before sending another")
				if result != nil {
					require.Nil(t, result.UpstreamRejection, "ordinary decrypt errors are not moderation charges")
				}
			} else {
				require.NoError(t, err)
				require.Len(t, upstream.requests, 2, "accounts outside this profile keep the existing one-repair behavior")
				require.NotNil(t, result)
				require.Nil(t, result.UpstreamRejection)
				require.Equal(t, 7, result.Usage.InputTokens)
			}
		})
	}
}

func TestUpstreamRejectionGatewayEarlyUsageThenCancellationIsUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prefix := strings.Join([]string{
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"Working on it"}}]}`, "",
		`data: {"model":"grok-4.7","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`, "", "",
	}, "\n")
	for _, scanError := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, configured := range []bool{true, false} {
			name := scanError.Error() + "/profile_on"
			if !configured {
				name = scanError.Error() + "/profile_off"
			}
			t.Run(name, func(t *testing.T) {
				account := rejectionGatewayTestAccount()
				if !configured {
					delete(account.Extra, "upstream_rejection_profile")
				}
				resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", prefix)
				resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(prefix), passthroughErrReadCloser{err: scanError}))
				upstream := &httpUpstreamRecorder{resp: resp}
				request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
				c, _ := rejectionGatewayContext("/v1/chat/completions", request)
				result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, account, request, "")
				require.NotNil(t, result)
				require.Nil(t, result.UpstreamRejection)
				require.Equal(t, 20, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens, "retain observed partial usage for reconciliation")
				if configured {
					require.Error(t, err)
					require.Empty(t, result.UpstreamTerminalEvent, "an early usage chunk cannot release a durable hold as a completed request")
					require.True(t, result.ClientDisconnect)
					requireNoRejectionFailover(t, err)
				} else {
					require.NoError(t, err, "non-profile accounts keep their existing partial-usage behavior")
				}
			})
		}
	}
}

func TestUpstreamRejectionGatewayAllChatChoicesMustFinish(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, allFinished := range []bool{false, true} {
		name := "one_unfinished"
		if allFinished {
			name = "all_finished"
		}
		t.Run(name, func(t *testing.T) {
			payload := strings.Join([]string{
				`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"First"},"finish_reason":"stop"},{"index":1,"delta":{"role":"assistant","content":"Second"}}]}`, "",
				`data: {"model":"grok-4.7","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`, "", "",
			}, "\n")
			if allFinished {
				payload += "data: {\"model\":\"grok-4.7\",\"choices\":[{\"index\":1,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
			}
			request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true,"n":2}`)
			c, _ := rejectionGatewayContext("/v1/chat/completions", request)
			upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)}
			result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, rejectionGatewayTestAccount(), request, "")
			require.NotNil(t, result)
			require.Nil(t, result.UpstreamRejection)
			if allFinished {
				require.NoError(t, err)
				require.Equal(t, "chat.completed", result.UpstreamTerminalEvent)
			} else {
				require.Error(t, err)
				require.Empty(t, result.UpstreamTerminalEvent)
				requireNoRejectionFailover(t, err)
			}
		})
	}
}

func TestUpstreamRejectionGatewayMalformedChatFinishCannotCompleteEarlyUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := strings.Join([]string{
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"Still working"}}]}`, "",
		`data: {"model":"grok-4.7","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`, "",
		// The outer closing brace is missing. GJSON can still find finish_reason,
		// but incomplete JSON is not proof that the supplier finished the request.
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`, "", "",
	}, "\n")
	request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, _ := rejectionGatewayContext("/v1/chat/completions", request)
	upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)}
	result, err := rejectionGatewayService(upstream).forwardAsRawChatCompletions(context.Background(), c, rejectionGatewayTestAccount(), request, "")
	require.NotNil(t, result)
	require.Nil(t, result.UpstreamRejection)
	require.Empty(t, result.UpstreamTerminalEvent)
	require.Error(t, err)
	requireNoRejectionFailover(t, err)
}

func TestUpstreamRejectionGatewayMalformedResponsesTerminalCannotCompleteEarlyUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_partial","model":"grok-4.7","status":"in_progress","output":[]}}`, "",
		`data: {"type":"response.output_text.delta","item_id":"msg_partial","output_index":0,"content_index":0,"delta":"Still working"}`, "",
		`data: {"type":"response.in_progress","response":{"id":"resp_partial","usage":{"input_tokens":7,"output_tokens":4}}}`, "",
		// A complete nested response with an incomplete event envelope.
		`data: {"type":"response.completed","response":{"id":"resp_partial","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4}}`, "", "",
	}, "\n")
	for _, protocol := range []string{"responses", "passthrough"} {
		for _, stream := range []bool{false, true} {
			name := protocol + "/buffered_sse"
			if stream {
				name = protocol + "/stream_sse"
			}
			t.Run(name, func(t *testing.T) {
				request, err := json.Marshal(map[string]any{"model": rejectionGatewayTestModel, "input": "hello", "stream": stream})
				require.NoError(t, err)
				c, _ := rejectionGatewayContext("/v1/responses", request)
				upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)}
				svc := rejectionGatewayService(upstream)
				var result *OpenAIForwardResult
				if protocol == "responses" {
					result, err = svc.forwardGrokResponses(context.Background(), c, rejectionGatewayTestAccount(), request, rejectionGatewayTestModel, stream, time.Now())
				} else {
					result, err = svc.forwardOpenAIPassthrough(context.Background(), c, rejectionGatewayTestAccount(), request, nil, rejectionGatewayTestModel, false, nil, stream, time.Now())
				}
				if result == nil {
					require.Error(t, err)
				} else {
					require.Nil(t, result.UpstreamRejection)
					require.Empty(t, result.UpstreamTerminalEvent, "only valid terminal JSON can release a durable reservation")
				}
				require.Len(t, upstream.requests, 1)
			})
		}
	}
}

func TestUpstreamRejectionGatewayCompletionKeepsStrongestTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"sentinel_only", []string{"[DONE]"}, "[DONE]"},
		{"completed_then_sentinel", []string{`{"type":"response.completed"}`, "[DONE]"}, "response.completed"},
		{"completed_then_alias_then_sentinel", []string{`{"type":"response.completed"}`, `{"type":"response.done"}`, "[DONE]"}, "response.completed"},
		{"sentinel_then_completed", []string{"[DONE]", `{"type":"response.completed"}`}, "response.completed"},
		{"done_alias_then_sentinel", []string{`{"type":"response.done"}`, "[DONE]"}, "response.done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := newUpstreamRejectionSSEObserver(rejectionGatewayTestAccount(), http.StatusOK)
			for _, event := range tc.events {
				observer.ObserveLine("data: " + event)
				observer.ObserveLine("")
			}
			observer.Finish()
			require.Equal(t, tc.want, observer.CompletionEvent())
		})
	}
}
