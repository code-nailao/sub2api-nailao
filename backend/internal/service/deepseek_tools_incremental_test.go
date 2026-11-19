//go:build unit

package service

import (
	"bufio"
	"errors"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestDeepSeekStreamFailurePreservesConfirmedTool(t *testing.T) {
	models, tools := 0, 0
	result, journal, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true}, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/tools/search") {
			tools++
			return deepSeekFixtureResponse(`{"results":[{"url":"https://example.com","title":"Source"}]}`), nil
		}
		models++
		if models == 1 {
			return deepSeekFixtureModelResponse(t, true, deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
		}
		return nil, errors.New("fixture continuation failed")
	})
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, tools)
	require.Equal(t, 1, result.DeepSeekTools.SearchCount)
	require.NotEmpty(t, journal.checkpoints)
	require.InDelta(t, .006, journal.checkpoints[len(journal.checkpoints)-1].Usage.BillingSnapshot.DeepSeekTools.Cost(), 1e-12)
	require.Greater(t, journal.checkpoints[len(journal.checkpoints)-1].Command.BalanceCost, .006)
	require.NotContains(t, response, "[DONE]")
	require.NotContains(t, response, "sources_web_")
	require.Contains(t, response, `"type":"error"`)
}

func TestDeepSeekStreamMixedCallsStayClientOwned(t *testing.T) {
	models, tools := 0, 0
	result, _, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "client_search", "parameters": map[string]any{"type": "object"}}}}}, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/tools/search") {
			tools++
			return deepSeekFixtureResponse(`{"results":[{"url":"https://example.com"}]}`), nil
		}
		models++
		body := deepSeekModelFixtureBody(t, r, "owned_1", "Python")
		body, e := sjson.Set(body, "choices.0.message.tool_calls.-1", map[string]any{"id": "client_1", "type": "function", "function": map[string]any{"name": "client_search", "arguments": "{\"x\":1}"}})
		require.NoError(t, e)
		return deepSeekFixtureModelResponse(t, true, body), nil
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, models)
	require.Equal(t, 1, tools)
	require.Contains(t, response, "client_search")
	require.Contains(t, response, "client_1")
	require.NotContains(t, response, "sources_web_")
	require.Equal(t, 1, strings.Count(response, "data: [DONE]"))
	require.Equal(t, 1, strings.Count(response, `"server_tool_use"`), "aggregate usage occurs once")
	require.Equal(t, 1, result.DeepSeekTools.SearchCount)
}

func TestDeepSeekStreamTruncationNeverCompletes(t *testing.T) {
	for _, tail := range []string{"", "data: [DONE]\n\n", "data: {bad}\n\n", "data: {\"error\":{\"message\":\"bad\"}}\n\n"} {
		t.Run(tail, func(t *testing.T) {
			result, _, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true}, func(r *http.Request) (*http.Response, error) {
				wire, e := io.ReadAll(r.Body)
				require.NoError(t, e)
				require.True(t, gjson.GetBytes(wire, "stream").Bool())
				data := `data: {"id":"partial","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"partial visible output"},"finish_reason":null}]}` + "\n\n" + tail
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
			})
			require.Error(t, err)
			require.NotNil(t, result)
			require.Contains(t, response, "partial visible output")
			require.NotContains(t, response, "[DONE]")
			require.NotNil(t, result.FirstTokenMs)
			require.False(t, result.ClientDisconnect, "upstream protocol error is not a client disconnect")
			require.Contains(t, response, `"type":"error"`)
		})
	}
}

func TestDeepSeekStreamOutputBudgetStopsBeforeTool(t *testing.T) {
	tools := 0
	result, _, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true, "max_completion_tokens": 5}, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/tools/") {
			tools++
			return nil, errors.New("must not execute")
		}
		return deepSeekFixtureModelResponse(t, true, deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
	})
	require.ErrorContains(t, err, "output token budget")
	require.Zero(t, tools)
	require.Equal(t, 10, result.Usage.OutputTokens)
	require.NotContains(t, response, "[DONE]")
}

type deepSeekGatedBody struct {
	first, tail *strings.Reader
	release     <-chan struct{}
}

func (b *deepSeekGatedBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	select {
	case <-b.release:
		return b.tail.Read(p)
	case <-time.After(5 * time.Second):
		return 0, errors.New("client never received first content")
	}
}

func TestDeepSeekHostedSocketStreamsBeforeUpstreamTerminal(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, _, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true}, func(req *http.Request) (*http.Response, error) {
			raw, e := io.ReadAll(req.Body)
			require.NoError(t, e)
			require.True(t, gjson.GetBytes(raw, "stream").Bool())
			first := `data: {"id":"socket","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"socket-first-token"},"finish_reason":null}]}` + "\n\n"
			tail := `data: {"id":"socket","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}` + "\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&deepSeekGatedBody{strings.NewReader(first), strings.NewReader(tail), release})}, nil
		}, func(c *gin.Context) {
			network, _ := gin.CreateTestContext(w)
			c.Writer = network.Writer
			c.Request = c.Request.WithContext(r.Context())
		})
		done <- err
	}))
	defer server.Close()
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	seen := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "socket-first-token") {
			seen = true
			break
		}
	}
	require.True(t, seen, "socket must deliver actual content before upstream is released")
	close(release)
	var tail strings.Builder
	for scanner.Scan() {
		tail.WriteString(scanner.Text())
		tail.WriteByte('\n')
	}
	require.NoError(t, scanner.Err())
	require.Contains(t, tail.String(), "[DONE]")
	require.NoError(t, <-done)
}

type deepSeekFailedClient struct{ gin.ResponseWriter }

func (w deepSeekFailedClient) Write([]byte) (int, error) {
	return 0, errors.New("fixture client disconnected")
}
func (w deepSeekFailedClient) WriteString(string) (int, error) {
	return 0, errors.New("fixture client disconnected")
}

func TestDeepSeekStreamWriteFailureDoesNotExecuteTools(t *testing.T) {
	tools := 0
	result, _, _, err := deepSeekHostedTestFixture(t, map[string]any{"stream": true}, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/tools/") {
			tools++
			return nil, errors.New("must not execute")
		}
		return deepSeekFixtureModelResponse(t, true, deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
	}, func(c *gin.Context) { c.Writer = deepSeekFailedClient{c.Writer} })
	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Zero(t, tools)
}
