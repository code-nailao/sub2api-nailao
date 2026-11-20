//go:build unit

package service

import (
	"fmt"
	"net/http"
	"testing"

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
		resultB  = `{"role":"tool","tool_call_id":"call_b","content":"Result B"}`
		orphan   = `{"role":"tool","tool_call_id":"unmatched","content":"Orphan"}`
		missingB = `{"role":"tool","tool_call_id":"call_b","content":""}`
	)
	cases := []struct {
		name, input, want string
	}{
		{"orphan", user + "," + orphan + "," + callA + "," + resultA + "," + followup, user + "," + callA + "," + resultA + "," + followup},
		{"missing", user + "," + callAB + "," + resultA + "," + followup, user + "," + callAB + "," + resultA + "," + missingB + "," + followup},
		{"out_of_order", resultB + "," + user + "," + callAB + "," + followup + "," + resultA, user + "," + callAB + "," + resultB + "," + resultA + "," + followup},
		{"already_paired", user + ",\n  " + callAB + ", \t" + resultB + ",\n\t" + resultA + ", " + followup, ""},
		{"reused_id_already_paired", user + "," + callA + ", " + resultA + "," + followup + "," + callA + ",\n" + resultA, ""},
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

func TestDeepSeekChatPairingScope(t *testing.T) {
	t.Run("another provider on the same raw Chat path", func(t *testing.T) {
		body := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"Example"},{"role":"tool","tool_call_id":"unmatched","content":"Original"}]}`)
		c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
		upstream := newOKChatCompletionsUpstream(t.Name(), deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		_, err := svc.forwardAsRawChatCompletions(c.Request.Context(), c, rawChatCompletionsTestAccount(), body, "")
		require.NoError(t, err)
		require.Equal(t, body, upstream.lastBody)
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
