//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDeepSeekChatPairsToolMessagesBeforeForwarding(t *testing.T) {
	const (
		user     = `{"role":"user","content":"Find an example"}`
		followup = `{"role":"user","content":"Use the result"}`
		callA    = `{ "role": "assistant", "content":null,"reasoning_content":"Keep the original reasoning","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"a\"}"}}] }`
		callAB   = `{"role":"assistant","content":"Checking","reasoning_content":"Keep the original reasoning","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
		resultA  = `{ "role":"t\u006fol", "tool_call_id":"call_\u0061", "content":"Result A [}\"" }`
		copyOfA  = `{"content":"Result \u0041 [}\u0022","tool_call_id":"call_a","role":"tool"}`
		resultB  = `{"role":"tool","tool_call_id":"call_b","content":"Result B"}`
		secondA  = `{"role":"tool","tool_call_id":"call_a","content":"Second result for A"}`
		thirdA   = `{"role":"tool","tool_call_id":"call_a","content":"Third result for A"}`
		orphan   = `{"role":"tool","tool_call_id":"unmatched","content":"Orphan"}`
		missingA = `{"role":"tool","tool_call_id":"call_a","content":""}`
		missingB = `{"role":"tool","tool_call_id":"call_b","content":""}`
	)
	cases := []struct {
		name, input, want string
	}{
		{"orphan", user + "," + orphan + "," + callA + "," + resultA + "," + followup, user + "," + callA + "," + resultA + "," + followup},
		{"missing", user + "," + callAB + "," + resultA + "," + followup, user + "," + callAB + "," + resultA + "," + missingB + "," + followup},
		{"out_of_order", resultB + "," + user + "," + callAB + "," + followup + "," + resultA, user + "," + callAB + "," + resultA + "," + resultB + "," + followup},
		{"parallel_first_result_missing", callAB + "," + resultB + "," + followup, callAB + "," + missingA + "," + resultB + "," + followup},
		{"parallel_first_result_displaced", callAB + "," + resultB + "," + followup + "," + resultA, callAB + "," + resultA + "," + resultB + "," + followup},
		{"already_paired", user + ",\n  " + callAB + ", \t" + resultB + ",\n\t" + resultA + ", " + followup, ""},
		{"reused_id_already_paired", user + "," + callA + ", " + resultA + "," + followup + "," + callA + ",\n" + resultA, ""},
		{"empty_id_in_call_batch_skips_repair", orphan + "," + strings.Replace(callAB, `"id":"call_b"`, `"id":""`, 1) + "," + resultA, ""},
		{"duplicate_id_in_call_batch_skips_repair", orphan + "," + strings.Replace(callAB, `"id":"call_b"`, `"id":"call_a"`, 1) + "," + resultA, ""},
		{
			"reused_id_ignores_duplicate_from_previous_call",
			callA + "," + resultA + "," + resultA + "," + callA + "," + followup + "," + secondA,
			callA + "," + resultA + "," + callA + "," + secondA + "," + followup,
		},
		{
			"reused_id_keeps_adjacent_result",
			callA + "," + resultA + "," + user + "," + callA + "," + resultA + "," + followup + "," + secondA,
			callA + "," + resultA + "," + user + "," + callA + "," + resultA + "," + followup,
		},
		{
			"reused_id_remembers_discarded_displaced_candidate",
			callA + "," + resultA + "," + callA + "," + resultA + "," + followup + "," + secondA + "," + callA + "," + secondA + "," + thirdA,
			callA + "," + resultA + "," + callA + "," + resultA + "," + followup + "," + callA + "," + thirdA,
		},
		{
			"reused_id_cross_boundary_duplicate",
			callA + "," + resultA + "," + callA + "," + resultA + "," + secondA,
			callA + "," + resultA + "," + callA + "," + secondA,
		},
		{
			"reused_id_remembers_discarded_candidates",
			callA + "," + resultA + "," + secondA + "," + callA + "," + secondA + "," + thirdA,
			callA + "," + resultA + "," + callA + "," + thirdA,
		},
		{
			"reused_id_displaced_cross_boundary_duplicate",
			callA + "," + resultA + "," + callA + "," + user + "," + copyOfA + "," + followup + "," + secondA,
			callA + "," + resultA + "," + callA + "," + secondA + "," + user + "," + followup,
		},
		{
			"reused_id_keeps_new_result_before_old_copy",
			callA + "," + resultA + "," + callA + "," + secondA + "," + copyOfA,
			callA + "," + resultA + "," + callA + "," + secondA,
		},
		{
			"reused_id_keeps_sole_displaced_result_with_same_content",
			callA + "," + resultA + "," + callA + "," + followup + "," + copyOfA,
			callA + "," + resultA + "," + callA + "," + copyOfA + "," + followup,
		},
		{
			"reused_id_both_displaced",
			callA + "," + user + "," + resultA + "," + callA + "," + followup + "," + secondA,
			callA + "," + resultA + "," + user + "," + callA + "," + secondA + "," + followup,
		},
		{
			"reused_id_ignores_duplicate_from_later_call",
			callA + "," + user + "," + callA + "," + secondA + "," + followup + "," + secondA,
			callA + "," + missingA + "," + user + "," + callA + "," + secondA + "," + followup,
		},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				wrap := func(messages string) []byte {
					return fmt.Appendf(nil, `{"model":"deepseek-flash", "messages":[%s], "stream":%t,"stream_options":{"include_usage":true}, "metadata":{"number":12345678901234567890}}`, messages, stream)
				}
				body := wrap(tc.input)
				want := body
				if tc.want != "" {
					want = wrap(tc.want)
				}
				for _, toolsEnabled := range []bool{false, true} {
					run := runDeepSeekOrdinaryChat(t, body, toolsEnabled)
					require.NoError(t, run.err)
					require.Equal(t, http.StatusOK, run.recorder.Code)
					require.Equal(t, stream, run.result.Stream)
					require.Equal(t, 1, run.calls)
					require.Equal(t, 1, run.billing.calls)
					require.Equal(t, string(want), string(run.sent))
				}
			})
		}
	}
}

func TestDeepSeekChatPairingPreservesAdjacentResultsWithReusedIDs(t *testing.T) {
	const call = `{"role":"assistant","reasoning_content":"Example reasoning","tool_calls":[{"id":"reused","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
	const first = `{"role":"tool","tool_call_id":"reused","content":"First result"}`
	const late = `{"role":"tool","tool_call_id":"reused","content":"Second result"}`
	const user = `{"role":"user","content":"Continue"}`
	body := []byte(`{"model":"deepseek-flash","messages":[` + call + `,` + first + `,` + call + `,` + user + `,` + late + `]}`)
	want := `{"model":"deepseek-flash","messages":[` + call + `,` + first + `,` + call + `,` + late + `,` + user + `]}`
	run := runDeepSeekOrdinaryChat(t, body, false)
	require.NoError(t, run.err)
	require.Equal(t, want, string(run.sent))
}

const deepSeekPairedChatFixture = `[
	  {"role":"user","content":"Example"},
	  { "role":"assistant", "content":null, "tool_calls":[{"id":"call_a","type":"function","function":{"name":"record","arguments":"{}"}}] },
	  {"role":"tool","tool_call_id":"call_a","content":"Result"}
	]`

func TestDeepSeekChatPairingKeepsAlignedHistory(t *testing.T) {
	cases := []struct {
		name, baseURL, model, options, wantThinking string
		wantPlaceholder                             bool
	}{
		{"thinking_disabled", DefaultDeepseekBaseURL, "deepseek-flash", `,"thinking":{"type":"disabled"}`, "disabled", false},
		{"forced_tool_disables_thinking", DefaultDeepseekBaseURL, "deepseek-flash", `,"thinking":{"type":"enabled"},"tool_choice":"required",` + deepSeekForcedToolChoiceTools, "disabled", false},
		{"glm_through_go", DefaultOpenCodeGoBaseURL, "glm-5", `,"thinking":{"type":"enabled"}`, "enabled", false},
		{"glm_through_zen", DefaultOpenCodeZenBaseURL, "glm-5", "", "", false},
		{"thinking_enabled", DefaultDeepseekBaseURL, "deepseek-flash", `,"thinking":{"type":"enabled"}`, "enabled", true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := fmt.Appendf(nil, `{"model":"fixture-model","messages":%s,"stream":%t%s}`, deepSeekPairedChatFixture, stream, tc.options)
				c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
				upstream := &httpUpstreamRecorder{resp: deepSeekFixtureModelResponse(t, stream, deepSeekChatFallbackOKBody)}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				account := deepSeekForcedToolChoiceAccount()
				account.Credentials["base_url"] = tc.baseURL
				account.Credentials["model_mapping"] = map[string]any{"fixture-model": tc.model}

				_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
				require.NoError(t, err)
				require.Equal(t, tc.model, gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, tc.wantThinking, gjson.GetBytes(upstream.lastBody, "thinking.type").String())
				if tc.wantPlaceholder {
					require.Equal(t, deepSeekChatReasoningPlaceholderText, gjson.GetBytes(upstream.lastBody, "messages.1.reasoning_content").String())
				} else {
					require.Equal(t, deepSeekPairedChatFixture, gjson.GetBytes(upstream.lastBody, "messages").Raw)
				}
			})
		}
	}
}

func TestDeepSeekChatThinkingAliasKeepsReasoningPlaceholder(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Appendf(nil, `{"model":"fixture-model","messages":%s,"thinking":{"type":"enabled"},"stream":%t}`, deepSeekPairedChatFixture, stream)
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			account := deepSeekForcedToolChoiceAccount()
			account.Credentials["model_mapping"] = map[string]any{"fixture-model": "thinking-alias"}
			sent := 0
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: deepSeekHTTPFixture{fn: func(req *http.Request) (*http.Response, error) {
				sent++
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "thinking-alias", gjson.GetBytes(wire, "model").String())
				require.Equal(t, "enabled", gjson.GetBytes(wire, "thinking.type").String())
				if gjson.GetBytes(wire, "messages.1.reasoning_content").String() == "" {
					resp := deepSeekFixtureResponse(`{"error":{"message":"fixture requires reasoning_content"}}`)
					resp.StatusCode = http.StatusBadRequest
					return resp, nil
				}
				return deepSeekFixtureModelResponse(t, stream, deepSeekChatFallbackOKBody), nil
			}}}

			_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
			require.Equal(t, 1, sent)
			require.NoError(t, err)
		})
	}
}

func TestDeepSeekChatAliasRepairsToolHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Appendf(nil, `{"model":"fixture-model","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"orphan","content":"Unmatched"},{"role":"assistant","content":null,"tool_calls":[{"id":"missing","type":"function","function":{"name":"record","arguments":"{}"}}]},{"role":"user","content":"Continue"}],"stream":%t}`, stream)
			c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
			account := deepSeekForcedToolChoiceAccount()
			account.Credentials["model_mapping"] = map[string]any{"fixture-model": "thinking-alias"}
			upstream := &httpUpstreamRecorder{resp: deepSeekFixtureModelResponse(t, stream, deepSeekChatFallbackOKBody)}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

			_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
			require.NoError(t, err)
			require.Equal(t, "thinking-alias", gjson.GetBytes(upstream.lastBody, "model").String())
			messages := gjson.GetBytes(upstream.lastBody, "messages").Array()
			require.Len(t, messages, 4)
			require.Equal(t, "assistant", messages[1].Get("role").String())
			require.Equal(t, deepSeekChatReasoningPlaceholderText, messages[1].Get("reasoning_content").String())
			require.Equal(t, `{"role":"tool","tool_call_id":"missing","content":""}`, messages[2].Raw)
		})
	}
}

func runDeepSeekHostedChatWithSearch(t *testing.T, cfg *config.Config, body []byte) (*OpenAIForwardResult, [][]byte, error) {
	t.Helper()
	stream := gjson.GetBytes(body, "stream").Bool()
	var requests [][]byte
	result, _, _, err := deepSeekHostedWireFixture(t, cfg, "chat", body, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/tools/search") {
			return deepSeekFixtureResponse(`{"results":[{"url":"https://source.example/page"}]}`), nil
		}
		require.True(t, strings.HasSuffix(req.URL.Path, "/chat/completions"))
		wire, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		requests = append(requests, wire)
		if len(requests) == 1 {
			req.Body = io.NopCloser(strings.NewReader(string(wire)))
			return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, req, "search_call", "example")), nil
		}
		return deepSeekFixtureModelResponse(t, stream, deepSeekChatFallbackOKBody), nil
	})
	return result, requests, err
}

func TestDeepSeekChatHostedKeepsPairedHistoryBytes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Appendf(nil, `{"model":"deepseek-flash","messages":%s,"thinking":{"type":"disabled"},"tools":[{"type":"web_search"}],"stream":%t}`, deepSeekPairedChatFixture, stream)
			result, requests, err := runDeepSeekHostedChatWithSearch(t, deepSeekChatFallbackTestConfig(), body)
			require.NoError(t, err)
			require.Equal(t, 1, result.DeepSeekTools.SearchCount)
			require.Len(t, requests, 2)
			require.Equal(t, deepSeekPairedChatFixture, gjson.GetBytes(requests[0], "messages").Raw)
			messages := gjson.GetBytes(requests[1], "messages").Array()
			require.Len(t, messages, 5)
			for i, message := range gjson.Parse(deepSeekPairedChatFixture).Array() {
				require.Equal(t, message.Raw, messages[i].Raw)
			}
			require.Equal(t, "search_call", messages[3].Get("tool_calls.0.id").String())
			require.Equal(t, "search_call", messages[4].Get("tool_call_id").String())
		})
	}
}

func TestDeepSeekChatHostedRepairKeepsCompleteBlock(t *testing.T) {
	const calls = `{ "role":"assistant","content":null, "tool_calls":[{"id":"a","type":"function","function":{"name":"record","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"record","arguments":"{}"}}] }`
	const resultB = `{ "role":"tool","tool_call_id":"b","content":"Result B" }`
	const resultA = `{ "role":"tool","tool_call_id":"a","content":"Result A" }`
	const block = calls + ",\n  " + resultB + ", " + resultA
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Appendf(nil, `{"model":"deepseek-flash","messages":[{"role":"user","content":"Example"},%s,{"role":"user","content":"Continue"},{"role":"tool","tool_call_id":"orphan","content":"Unmatched"}],"thinking":{"type":"disabled"},"tools":[{"type":"web_search"}],"stream":%t}`, block, stream)
			result, requests, err := runDeepSeekHostedChatWithSearch(t, deepSeekChatFallbackTestConfig(), body)
			require.NoError(t, err)
			require.Equal(t, 1, result.DeepSeekTools.SearchCount)
			require.Len(t, requests, 2)
			for _, wire := range requests {
				messages := gjson.GetBytes(wire, "messages")
				require.False(t, messages.Get(`#(tool_call_id=="orphan")`).Exists())
				require.Contains(t, messages.Raw, block)
			}
		})
	}
}

func TestDeepSeekChatHostedContinuationBodyBudget(t *testing.T) {
	const limit = 4096
	const prompt = "Find an example with  two spaces"
	const bodyFormat = `{"model":"deepseek-flash","messages":[%s{"role":"user","content":%q}],"thinking":{"type":"disabled"},"tools":[{"type":"web_search"}],"stream":%t}`
	for _, stream := range []bool{false, true} {
		for _, insideString := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/padding_inside_string=%t", stream, insideString), func(t *testing.T) {
				cfg := deepSeekChatFallbackTestConfig()
				cfg.Gateway.MaxBodySize = limit
				cfg.Gateway.TextMaxBodySize = limit
				padding := strings.Repeat(" ", limit-850-len(fmt.Sprintf(bodyFormat, "", prompt, stream)))
				content := prompt
				if insideString {
					content += padding
					padding = ""
				}
				body := fmt.Appendf(nil, bodyFormat, padding, content, stream)
				result, requests, err := runDeepSeekHostedChatWithSearch(t, cfg, body)
				if insideString {
					require.ErrorContains(t, err, "DeepSeek conversation exceeds request body limit")
					require.Len(t, requests, 1)
				} else {
					require.NoError(t, err)
					require.Len(t, requests, 2)
					require.Equal(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(requests[0], "messages").Raw)
				}
				require.Equal(t, 1, result.DeepSeekTools.SearchCount)
				for _, wire := range requests {
					require.LessOrEqual(t, len(wire), limit)
					require.Equal(t, content, gjson.GetBytes(wire, "messages.0.content").String())
				}
			})
		}
	}
}

func TestDeepSeekChatPairingOpenCodeGo(t *testing.T) {
	const messages = `[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"unmatched","content":"Original"}]`
	body := []byte(`{"model":"fixture-model","messages":` + messages + `}`)
	for _, platform := range []string{PlatformOpenCodeGo, PlatformOpenAI} {
		for _, tc := range []struct {
			model        string
			wantMessages string
		}{
			{"deepseek-v4-flash", `[{"role":"user","content":"Example"}]`},
			{"glm-5", messages},
		} {
			t.Run(platform+"/"+tc.model, func(t *testing.T) {
				c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
				upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				account := openaiPlatformDeepSeekAccount()
				account.Platform = platform
				account.Credentials["base_url"] = DefaultOpenCodeGoBaseURL
				account.Credentials["model_mapping"] = map[string]any{"fixture-model": tc.model}

				_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
				require.NoError(t, err)
				require.Equal(t, buildOpenAIChatCompletionsURL(DefaultOpenCodeGoBaseURL), upstream.lastReq.URL.String())
				require.Equal(t, tc.model, gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, tc.wantMessages, gjson.GetBytes(upstream.lastBody, "messages").Raw)
			})
		}
	}
}

func TestDeepSeekChatPairingScope(t *testing.T) {
	t.Run("another provider on the same raw Chat path", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"unmatched","content":"Original"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		_, err := svc.forwardAsRawChatCompletions(c.Request.Context(), c, rawChatCompletionsTestAccount(), body, "")
		require.NoError(t, err)
		require.Equal(t, body, upstream.lastBody)
	})
	t.Run("GLM on a DeepSeek OpenCode Go mount", func(t *testing.T) {
		body := []byte(`{"model":"glm-5","messages":[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"unmatched","content":"Original"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		account := deepSeekForcedToolChoiceAccount()
		account.Credentials["base_url"] = DefaultOpenCodeGoBaseURL

		_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, "glm-5", gjson.GetBytes(upstream.lastBody, "model").String())
		require.Equal(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(upstream.lastBody, "messages").Raw)
	})
	t.Run("mapped DeepSeek model on an OpenAI account", func(t *testing.T) {
		body := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"unmatched","content":"Original"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		account := openaiPlatformDeepSeekAccount()
		account.Credentials["model_mapping"] = map[string]any{"fixture-model": "deepseek-flash"}

		_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, "deepseek-flash", gjson.GetBytes(upstream.lastBody, "model").String())
		require.Equal(t, `[{"role":"user","content":"Example"}]`, gjson.GetBytes(upstream.lastBody, "messages").Raw)
	})
	t.Run("Responses-shaped input on the Chat endpoint", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-flash","input":[{"role":"user","content":"Example"},{"role":"tool","content":"Original tool result"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
		account := adaptiveProtocolTestAccount(PlatformZhipu, map[string]any{
			APIProtocolChatCompletions: DefaultDeepseekBaseURL,
		})

		_, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
		tool := gjson.GetBytes(upstream.lastBody, `messages.#(role=="tool")`)
		require.Equal(t, "Original tool result", tool.Get("content").String())
		require.False(t, tool.Get("tool_call_id").Exists())
	})
	t.Run("Responses forwarding to DeepSeek Chat", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-flash","input":[{"role":"user","content":"Example"},{"role":"assistant","content":"Original"},{"type":"function_call","call_id":"unmatched","name":"lookup","arguments":"{}"},{"role":"user","content":"Continue"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/responses", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
		_, err := svc.Forward(c.Request.Context(), c, deepSeekForcedToolChoiceAccount(), body)
		require.NoError(t, err)
		// The existing Responses converter removes unanswered calls while
		// retaining assistant content. Chat's placeholder policy is separate.
		assistant := gjson.GetBytes(upstream.lastBody, `messages.#(role=="assistant")`)
		require.Equal(t, "Original", assistant.Get("content").String())
		require.False(t, assistant.Get("tool_calls").Exists())
		require.False(t, gjson.GetBytes(upstream.lastBody, `messages.#(role=="tool")`).Exists())
	})
}
