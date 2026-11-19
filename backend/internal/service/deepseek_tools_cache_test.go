//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A continuing conversation must not change its tool prefix merely because
// another HTTP request has begun. No provider or customer prompts are replayed.
func TestDeepSeekHostedCacheStableToolsAcrossRequests(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			body := []byte(`{"model":"deepseek-flash","tools":[],"messages":[{"role":"user","content":"synthetic cache fixture"}],"input":"synthetic cache fixture"}`)
			first, handled, err := prepareDeepSeekHosted(body, protocol, true)
			require.NoError(t, err)
			require.True(t, handled)
			want, err := json.Marshal(first.Body["tools"])
			require.NoError(t, err)
			for i := 0; i < 3; i++ {
				var continuation map[string]any
				require.NoError(t, json.Unmarshal(body, &continuation))
				continuation["messages"] = append(continuation["messages"].([]any), map[string]any{"role": "user", "content": fmt.Sprintf("synthetic continuation %d", i)})
				continuation["input"] = fmt.Sprintf("synthetic cache fixture continuation %d", i)
				wire, err := json.Marshal(continuation)
				require.NoError(t, err)
				next, handled, err := prepareDeepSeekHosted(wire, protocol, true)
				require.NoError(t, err)
				require.True(t, handled)
				got, err := json.Marshal(next.Body["tools"])
				require.NoError(t, err)
				require.Equal(t, string(want), string(got), "tool names/schema must remain stable across requests for prefix caching")
			}
		})
	}
}

func TestDeepSeekHostedCacheNamesAvoidClientTools(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			clientNames := []string{"sources_web_search_v1", "sources_web_search_v1_1", "sources_web_fetch_v1"}
			var tools []any
			for _, name := range clientNames {
				tool := map[string]any{"name": name, "parameters": map[string]any{"type": "object"}}
				switch protocol {
				case "chat":
					tool = map[string]any{"type": "function", "function": tool}
				case "responses":
					tool["type"] = "function"
				case "messages":
					delete(tool, "parameters")
					tool["input_schema"] = map[string]any{"type": "object"}
				}
				tools = append(tools, tool)
			}
			body, err := json.Marshal(map[string]any{"tools": tools})
			require.NoError(t, err)
			r, handled, err := prepareDeepSeekHosted(body, protocol, true)
			require.NoError(t, err)
			require.True(t, handled)
			require.Len(t, r.Owned, 2)
			for _, name := range clientNames {
				require.NotContains(t, r.Owned, name, "a client function must never become a gateway-owned operation")
				var response any
				switch protocol {
				case "chat":
					response = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"type": "function", "id": "client-call", "function": map[string]any{"name": name, "arguments": "{}"}}}}}}}
				case "messages":
					response = map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "client-call", "name": name, "input": map[string]any{}}}}
				default:
					response = map[string]any{"output": []any{map[string]any{"type": "function_call", "call_id": "client-call", "name": name, "arguments": "{}"}}}
				}
				wire, err := json.Marshal(response)
				require.NoError(t, err)
				owned, client := r.calls(wire)
				require.Empty(t, owned)
				require.True(t, client)
			}
			firstTools := r.Body["tools"].([]any)
			want, err := json.Marshal(tools)
			require.NoError(t, err)
			got, err := json.Marshal(firstTools[:len(tools)])
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
			next, _, err := prepareDeepSeekHosted(body, protocol, true)
			require.NoError(t, err)
			require.Equal(t, r.Owned, next.Owned, "collision resolution must also be stable")
		})
	}
}

// This fake provider models a cache partitioned by the tool schema. The request
// goes through the real hosted forwarder and real DeepSeek usage/billing parser.
func TestDeepSeekHostedRepeatedRequestPreservesCacheUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			seen := map[string]bool{}
			modelCalls := 0
			upstream := func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "/v1/chat/completions", req.URL.Path)
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				tools := gjson.GetBytes(wire, "tools").Raw
				require.NotEmpty(t, tools)
				cached := 0
				if seen[tools] {
					cached = 80
				}
				seen[tools] = true
				modelCalls++
				return deepSeekFixtureModelResponse(t, stream, fmt.Sprintf(`{"id":"fixture-%d","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":%d,"prompt_cache_miss_tokens":%d}}`, modelCalls, cached, 100-cached)), nil
			}
			first, _, _, err := deepSeekHostedTestFixture(t, map[string]any{"stream": stream}, upstream)
			require.NoError(t, err)
			require.Zero(t, first.Usage.CacheReadInputTokens)
			second, journal, response, err := deepSeekHostedTestFixture(t, map[string]any{"stream": stream}, upstream)
			require.NoError(t, err)
			require.Equal(t, 2, modelCalls)
			require.Equal(t, 80, second.Usage.CacheReadInputTokens, "the warmed tool prefix must survive a new hosted request")
			require.NotEmpty(t, journal.checkpoints)
			require.Equal(t, 80, journal.checkpoints[len(journal.checkpoints)-1].Usage.CacheReadTokens)
			require.Contains(t, response, `"cached_tokens":80`)
		})
	}
}
