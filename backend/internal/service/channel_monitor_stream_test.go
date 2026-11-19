//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func streamingProbeOptions(apiMode string) *CheckOptions {
	body := map[string]any{"model": "gpt-6-sol", "stream": true}
	if apiMode == MonitorAPIModeResponses {
		body["instructions"] = "Reply briefly."
		body["input"] = "Reply with exactly: OK"
		body["max_output_tokens"] = 20
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
	}
	return &CheckOptions{APIMode: apiMode, BodyOverrideMode: MonitorBodyOverrideModeReplace, BodyOverride: body}
}

const monitorCompletedFixture = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"

func TestRunCheckForModel_StreamingResponses_Valid(t *testing.T) {
	swapMonitorHTTPClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true || body["model"] != "gpt-6-sol" || r.URL.Path != providerOpenAIResponsesPath {
			t.Errorf("unexpected streaming request: %v %s", body, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"O\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"K\"}\n\n")
		_, _ = fmt.Fprint(w, monitorCompletedFixture)
	}))
	defer srv.Close()
	result := runCheckForModel(context.Background(), MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(MonitorAPIModeResponses))
	require.Equal(t, MonitorStatusOperational, result.Status, result.Message)
	require.NotNil(t, result.LatencyMs)
}

func TestMonitorStreamingResponseText(t *testing.T) {
	chatEnd := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		name, apiMode, wire, text string
	}{
		{"responses final snapshot avoids duplicate deltas", MonitorAPIModeResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" + monitorCompletedFixture, "OK"},
		{"responses delta fallback", MonitorAPIModeResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"O\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"K\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", "OK"},
		{"responses leading reasoning", MonitorAPIModeResponses, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\"},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n", "OK"},
		{"bare CR delimiters", MonitorAPIModeResponses, strings.ReplaceAll(monitorCompletedFixture, "\n", "\r"), "OK"},
		{"CRLF multiline data heartbeat BOM", MonitorAPIModeResponses, "\xef\xbb\xbf: heartbeat\r\n\r\nid: 1\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output_text\":\"你好\"}}\r\n\r\n", "你好"},
		{"chat chunks and usage", MonitorAPIModeChatCompletions, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":5}}\n\n" + chatEnd, "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapMonitorHTTPClient(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("stream request Accept = %q", r.Header.Get("Accept"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				// Deliberately split UTF-8 and SSE fields across network writes.
				for wire := tc.wire; wire != ""; {
					n := min(3, len(wire))
					_, _ = fmt.Fprint(w, wire[:n])
					w.(http.Flusher).Flush()
					wire = wire[n:]
				}
			}))
			defer srv.Close()
			text, _, status, err := callProvider(context.Background(), MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", "unused", streamingProbeOptions(tc.apiMode))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, tc.text, text)
		})
	}
}

func TestMonitorStreamingFailuresDoNotBecomeHealthy(t *testing.T) {
	for _, tc := range []struct {
		name, apiMode, contentType, wire, message string
		status                                    int
	}{
		{"incomplete token budget", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", "max_output_tokens", 200},
		{"failure after visible text", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"upstream failed\"}}}\n\n", "upstream failed", 200},
		{"early EOF", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n", "before completion", 200},
		{"missing SSE delimiter", MonitorAPIModeResponses, "text/event-stream", strings.TrimRight(monitorCompletedFixture, "\n"), "before completion", 200},
		{"empty stream", MonitorAPIModeResponses, "text/event-stream", ": heartbeat\n\n", "before completion", 200},
		{"malformed JSON", MonitorAPIModeResponses, "text/event-stream", "data: {bad}\n\n", "invalid JSON", 200},
		{"malformed event shape", MonitorAPIModeResponses, "text/event-stream", "data: []\n\n", "JSON object", 200},
		{"conflicting event type", MonitorAPIModeResponses, "text/event-stream", "event: response.failed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", "event type", 200},
		{"completion status mismatch", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n", "status", 200},
		{"response id changed", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"another\"}}\n\n" + monitorCompletedFixture, "response ID", 200},
		{"done is not Responses completion", MonitorAPIModeResponses, "text/event-stream", "data: [DONE]\n\n", "before completion", 200},
		{"JSON instead of stream", MonitorAPIModeResponses, "application/json", "{\"output_text\":\"OK\"}", "expected text/event-stream", 200},
		{"HTML instead of stream", MonitorAPIModeResponses, "text/html", "<html>OK</html>", "expected text/event-stream", 200},
		{"upstream HTTP failure", MonitorAPIModeResponses, "application/json", "{\"error\":{\"message\":\"rate limited\"}}", "upstream HTTP 429", 429},
		{"bounded heartbeat stream", MonitorAPIModeResponses, "text/event-stream", strings.Repeat(": heartbeat\n\n", monitorResponseMaxBytes/13+1) + monitorCompletedFixture, "size limit", 200},
		{"chat missing stop", MonitorAPIModeChatCompletions, "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n", "before completion", 200},
		{"chat missing done", MonitorAPIModeChatCompletions, "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n", "before completion", 200},
		{"chat length finish", MonitorAPIModeChatCompletions, "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "length", 200},
		{"chat error event", MonitorAPIModeChatCompletions, "text/event-stream", "event: error\ndata: {\"error\":{\"message\":\"provider unavailable\"}}\n\n", "provider unavailable", 200},
		{"redacted streaming error", MonitorAPIModeResponses, "text/event-stream", "data: {\"type\":\"error\",\"message\":\"invalid sk-abcdefghijklmnopqrstuvwxyz\"}\n\n", "sk-***REDACTED***", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapMonitorHTTPClient(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.wire)
			}))
			defer srv.Close()
			result := runCheckForModel(context.Background(), MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(tc.apiMode))
			require.Equal(t, MonitorStatusError, result.Status, result.Message)
			require.Contains(t, result.Message, tc.message)
		})
	}
}

func TestMonitorStreamingCompletionDoesNotWaitForHTTPEOF(t *testing.T) {
	swapMonitorHTTPClient(t)
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, monitorCompletedFixture)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()
	ctx := withChannelMonitorProbeTiming(context.Background(), channelMonitorProbeTiming{DegradedThreshold: time.Second, RequestTimeout: 2 * time.Second})
	result := runCheckForModel(ctx, MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(MonitorAPIModeResponses))
	require.Equal(t, MonitorStatusOperational, result.Status, result.Message)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("response body not closed after terminal event")
	}
}

func TestMonitorStreamingTimeoutIncludesBody(t *testing.T) {
	swapMonitorHTTPClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx := withChannelMonitorProbeTiming(context.Background(), channelMonitorProbeTiming{DegradedThreshold: 10 * time.Millisecond, RequestTimeout: 80 * time.Millisecond})
	result := runCheckForModel(ctx, MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(MonitorAPIModeResponses))
	require.Equal(t, MonitorStatusError, result.Status)
	require.Contains(t, result.Message, "context deadline exceeded")
	require.Equal(t, 5*time.Second, monitorHTTPClient.Timeout)
}

func TestMonitorStreamingUsesTotalLatency(t *testing.T) {
	swapMonitorHTTPClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, monitorCompletedFixture)
	}))
	defer srv.Close()
	ctx := withChannelMonitorProbeTiming(context.Background(), channelMonitorProbeTiming{DegradedThreshold: 20 * time.Millisecond, RequestTimeout: time.Second})
	result := runCheckForModel(ctx, MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(MonitorAPIModeResponses))
	require.Equal(t, MonitorStatusDegraded, result.Status, result.Message)
	require.GreaterOrEqual(t, *result.LatencyMs, 20)
}

func TestMonitorStreamingInvalidOptionFailsBeforeNetwork(t *testing.T) {
	for _, value := range []any{"true", 1, nil} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			swapMonitorHTTPClient(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer srv.Close()
			opts := streamingProbeOptions(MonitorAPIModeResponses)
			opts.BodyOverride["stream"] = value
			result := runCheckForModel(context.Background(), MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", opts)
			require.Equal(t, MonitorStatusError, result.Status)
			require.Contains(t, result.Message, "stream must be a boolean")
			require.Zero(t, calls.Load())
		})
	}
}

func TestMonitorStreamingCompletedEmptyTextFails(t *testing.T) {
	swapMonitorHTTPClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer srv.Close()
	result := runCheckForModel(context.Background(), MonitorProviderOpenAI, srv.URL, "sk-test", "gpt-6-sol", streamingProbeOptions(MonitorAPIModeResponses))
	require.Equal(t, MonitorStatusFailed, result.Status)
}
