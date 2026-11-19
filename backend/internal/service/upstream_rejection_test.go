//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func rejectionTestAccount() *Account {
	return &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}}
}
func rejectionChatBody(text string) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}}}})
	return b
}
func rejectionDelta(text string, index int) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": index, "delta": map[string]any{"content": text}}}})
	return b
}

func TestUpstreamRejectionAccountOptIn(t *testing.T) {
	for name, a := range map[string]*Account{
		"nil": nil, "default": {Platform: PlatformGrok},
		"other_platform":   {Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}},
		"oauth_account":    {Platform: PlatformGrok, Type: AccountTypeOAuth, Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}},
		"setup_account":    {Platform: PlatformGrok, Type: "setup-token", Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}},
		"upstream_account": {Platform: PlatformGrok, Type: "upstream", Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}},
		"untyped_account":  {Platform: PlatformGrok, Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}},
		"unknown_profile":  {Platform: PlatformGrok, Type: AccountTypeAPIKey, Extra: map[string]any{"upstream_rejection_profile": "auto"}},
		"wrong_type":       {Platform: PlatformGrok, Type: AccountTypeAPIKey, Extra: map[string]any{"upstream_rejection_profile": true}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, a.GetUpstreamRejectionProfile())
			require.Nil(t, DetectUpstreamRejectionHTTP(a, 403, []byte(`{"error":"content moderation"}`)))
		})
	}
	a := rejectionTestAccount()
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, a.GetUpstreamRejectionProfile())
	require.Equal(t, 0.1, a.GetUpstreamRejectionCost())
	require.Equal(t, "USD", a.GetUpstreamRejectionCurrency())
	for _, cost := range []any{0.2, float32(.2), int(1), int64(1), "0.2", json.Number("0.2")} {
		a.Extra["upstream_rejection_cost"] = cost
		require.Positive(t, a.GetUpstreamRejectionCost())
	}
	for _, cost := range []any{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, struct{}{}, "bad", json.Number("bad"), nil} {
		a.Extra["upstream_rejection_cost"] = cost
		require.True(t, math.IsNaN(a.GetUpstreamRejectionCost()), "invalid supplier cost must not become free")
	}
	for _, cost := range []any{float64(0), int(0), "0", json.Number("0")} {
		a.Extra["upstream_rejection_cost"] = cost
		require.Zero(t, a.GetUpstreamRejectionCost(), "explicit zero supplier cost is legitimate")
	}
}

func TestUpstreamRejectionAllPhraseSuffixCombinations(t *testing.T) {
	for _, phrase := range upstreamRejectionPhrases {
		for _, suffix := range upstreamRejectionSuffixes {
			for _, status := range []int{200, 201, 206, 299, 403} {
				text := strings.ToUpper(phrase) + " " + suffix + "thing" // No word boundary is required.
				name := fmt.Sprintf("%d/%s/%s", status, phrase, suffix)
				t.Run(name, func(t *testing.T) {
					b := rejectionChatBody(text)
					if status == 403 {
						b, _ = json.Marshal(map[string]any{"error": map[string]any{"message": text}})
					}
					e := DetectUpstreamRejectionHTTP(rejectionTestAccount(), status, b)
					require.NotNil(t, e)
					require.Equal(t, status, e.HTTPStatus)
					require.Equal(t, UpstreamRejectionRuleVersion(), e.RuleVersion)
					require.Len(t, e.ResponseHash, 64)
				})
			}
		}
	}
	for _, text := range []string{"I can’t help with that", "I can‘t help with this", "I’m sorry, I can’t help with your prompt", "I cannot help withitinerary", "I cannot help with\t\nthat", "prefix: I can't help with that"} {
		require.NotNil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, rejectionChatBody(text)), text)
	}
}

func TestUpstreamRejectionLiteralNegativePhrases(t *testing.T) {
	for _, text := range []string{"I can help with that", "I can't help with another", "Sorry, I cannot help you complete this request.", "No. I won't repeat an exact phrase on demand.", "I can't help with: that", "I can't help with", "I c***t help with that", "I ca*** help with that", "I can't  help with that", "I cannot help with yesterday"} {
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, rejectionChatBody(text)), text)
	}
}

func TestUpstreamRejectionHTTPStatusMatrix(t *testing.T) {
	for _, status := range []int{0, 199, 300, 401, 402, 404, 429, 500, 502, 503} {
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), status, []byte(`{"error":{"message":"content policy"}}`)), status)
	}
	for _, phrase := range []string{"generated image rejected by content moderation", "generated video rejected by content moderation"} {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": strings.ToUpper(phrase)}})
		require.NotNil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 400, body))
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, body))
	}
	require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 400, []byte(`{"error":"content moderation"}`)))
	for _, phrase := range upstreamRejection403Phrases {
		t.Run(phrase, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": phrase}})
			require.NotNil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 403, body))
		})
	}
	for _, body := range []string{
		`{"error":"invalid API key"}`, `{"error":"subscription required"}`, `{"error":"the moderation feature is not available"}`,
		`{"error":{"message":"bad request","request":{"prompt":"content moderation"}}}`,
		`{"input":"content moderation","metadata":"content policy"}`,
		`{"error":{"details":{"original_request":"content policy"}}}`,
		`<html><body>content moderation</body></html>`, `<!DOCTYPE html>content policy`,
		`{"error":"content moderation"`,
	} {
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 403, []byte(body)), body)
	}
	require.NotNil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 403, []byte("Content violates usage guidelines")))
	require.NotNil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 403, []byte(`{"response":{"error":{"message":"content moderation"}}}`)))
}

func TestUpstreamRejectionOnlyAssistantOutput(t *testing.T) {
	text := "I cannot help with that request"
	for _, body := range []string{
		`{"input":"I cannot help with that request"}`,
		`{"choices":[{"message":{"role":"user","content":"I cannot help with that request"}}]}`,
		`{"choices":[{"message":{"role":"tool","content":"I cannot help with that request"}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"function":{"arguments":"I cannot help with that request"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","reasoning_content":"I cannot help with that request"}}]}`,
		`{"output":[{"type":"function_call","arguments":"I cannot help with that request"}]}`,
		`{"output":[{"type":"reasoning","summary":[{"type":"text","text":"I cannot help with that request"}]}]}`,
		`{"output":[{"type":"message","role":"user","content":[{"type":"output_text","text":"I cannot help with that request"}]}]}`,
		`{"data":[{"b64_json":"I cannot help with that request"}]}`,
		`{"error":{"message":"I cannot help with that request","code":"content_policy"}}`,
	} {
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, []byte(body)), body)
	}
	for _, body := range []string{
		`{"output_text":"I cannot help with that request"}`,
		`{"choices":[{"message":{"role":"assistant","refusal":"I cannot help with that request"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"I cannot help with that request"}]}}]}`,
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I cannot help with that request"}]}]}`,
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that request"}]}]}`,
	} {
		e := DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, []byte(body))
		require.NotNil(t, e, body)
		encoded, _ := json.Marshal(e)
		require.NotContains(t, string(encoded), text)
	}
}

func TestUpstreamRejectionStreamingAllSplitPoints(t *testing.T) {
	text := "I'm sorry, I cannot help with that request."
	for split := 0; split <= len(text); split++ {
		t.Run(strconvItoa(split), func(t *testing.T) {
			o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
			o.ObserveSSEData(rejectionDelta(text[:split], 0))
			o.ObserveSSEData(rejectionDelta(text[split:], 0))
			require.NotNil(t, o.Evidence())
			before := *o.Evidence()
			o.ObserveJSON(rejectionChatBody(text))
			require.Equal(t, before, *o.Evidence())
			copy := o.Evidence()
			copy.RuleID = "tampered"
			require.Equal(t, before, *o.Evidence())
		})
	}
}
func strconvItoa(n int) string { return fmt.Sprintf("%d", n) }

func TestUpstreamRejectionStreamingIsolationAndBounds(t *testing.T) {
	o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
	o.ObserveSSEData(rejectionDelta("I cannot help with ", 0))
	o.ObserveSSEData(rejectionDelta("that", 1))
	require.Nil(t, o.Evidence())
	o.ObserveSSEData(rejectionDelta("another", 0))
	require.Nil(t, o.Evidence())
	o = NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
	for i := 0; i < 100; i++ {
		o.ObserveText(fmt.Sprint(i), strings.Repeat("a", 10000), false)
	}
	require.LessOrEqual(t, len(o.tails), upstreamRejectionMaxOutputs)
	for _, tail := range o.tails {
		require.LessOrEqual(t, len(tail), upstreamRejectionTextTail)
	}
	o = NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
	o.ObserveText("0", "I cannot help with", false)
	for i := 0; i < 10; i++ {
		o.ObserveText("0", strings.Repeat(" ", 1000), false)
	}
	o.ObserveText("0", "th", false)
	o.ObserveText("0", "at", false)
	require.NotNil(t, o.Evidence())
}

func TestUpstreamRejectionResponsesEvents(t *testing.T) {
	for _, typ := range []string{"response.output_text.delta", "response.refusal.delta"} {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		for _, chunk := range []string{"I can", "not help with ", "this"} {
			b, _ := json.Marshal(map[string]any{"type": typ, "output_index": 0, "content_index": 0, "delta": chunk})
			o.ObserveSSEData(b)
		}
		require.NotNil(t, o.Evidence())
	}
	for _, body := range []string{
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"I cannot help with this"}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"I cannot help with this"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I cannot help with this"}]}}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"I cannot help with this"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I cannot help with this"}]}]}}`,
	} {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData([]byte(body))
		require.NotNil(t, o.Evidence(), body)
	}
	for _, body := range []string{
		`{"type":"response.function_call_arguments.delta","delta":"I cannot help with that"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"I cannot help with that"}`,
		`{"type":"error","error":{"message":"I cannot help with that","code":"content_moderation"}}`,
		`{"type":"response.failed","response":{"error":{"message":"generated video rejected by content moderation"}}}`,
		`{"choices":[{"index":0,"delta":{"role":"tool","content":"I cannot help with that"}}]}`,
		`[DONE]`, `{invalid`,
	} {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData([]byte(body))
		require.Nil(t, o.Evidence(), body)
	}
}

func TestUpstreamRejectionObserverNilAndDisabled(t *testing.T) {
	var nilObserver *UpstreamRejectionObserver
	for _, o := range []*UpstreamRejectionObserver{nilObserver, NewUpstreamRejectionObserver(nil, 200), NewUpstreamRejectionObserver(rejectionTestAccount(), 403)} {
		o.ObserveJSON(rejectionChatBody("I cannot help with that"))
		o.ObserveSSEData(rejectionDelta("I cannot help with that", 0))
		o.ObserveText("0", "I cannot help with that", false)
		require.Nil(t, o.Evidence())
	}
}

func TestUpstreamRejectionResponsesMissingIndexIsolation(t *testing.T) {
	event := func(item, text string) []byte {
		m := map[string]any{"type": "response.output_text.delta", "delta": text}
		if item != "" {
			m["item_id"] = item
		}
		b, _ := json.Marshal(m)
		return b
	}
	t.Run("distinct_items_never_join", func(t *testing.T) {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData(event("item-a", "I cannot help with "))
		o.ObserveSSEData(event("item-b", "that"))
		require.Nil(t, o.Evidence())
		o.ObserveSSEData(event("item-a", "that request"))
		require.NotNil(t, o.Evidence())
	})
	t.Run("missing_identity_never_joins", func(t *testing.T) {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData(event("", "I cannot help with "))
		o.ObserveSSEData(event("", "that"))
		require.Nil(t, o.Evidence())
		o.ObserveSSEData(event("", "I cannot help with that"))
		require.NotNil(t, o.Evidence())
	})
	t.Run("distinct_content_parts_never_join", func(t *testing.T) {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData([]byte(`{"type":"response.output_text.delta","item_id":"x","content_index":0,"delta":"I cannot help with "}`))
		o.ObserveSSEData([]byte(`{"type":"response.output_text.delta","item_id":"x","content_index":1,"delta":"that"}`))
		require.Nil(t, o.Evidence())
	})
	t.Run("item_added_prefix_and_delta", func(t *testing.T) {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData([]byte(`{"type":"response.output_item.added","item":{"id":"x","type":"message","role":"assistant","content":[{"type":"output_text","text":"I cannot help with "}]}}`))
		o.ObserveSSEData(event("x", "that"))
		require.NotNil(t, o.Evidence())
	})
}

func TestUpstreamRejectionFullRoleCacheDoesNotTreatToolAsAssistant(t *testing.T) {
	o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
	for i := 0; i < upstreamRejectionMaxOutputs; i++ {
		o.ObserveSSEData(rejectionDelta("hello", i))
	}
	for _, role := range []string{"tool", "user"} {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 99, "delta": map[string]any{"role": role, "content": "I cannot help with that"}}}})
		o.ObserveSSEData(b)
		require.Nil(t, o.Evidence())
		o.ObserveSSEData(rejectionDelta("I cannot help with that", 99))
		require.Nil(t, o.Evidence())
	}
	require.LessOrEqual(t, len(o.roles), upstreamRejectionMaxOutputs)
	// Complete validated assistant messages remain detectable after the delta cap.
	o.ObserveJSON(rejectionChatBody("I cannot help with that"))
	require.NotNil(t, o.Evidence())
}

func TestUpstreamRejectionDoesNotStringifyStructuredText(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"message":{"content":[{"type":"text","text":{"prompt":"I cannot help with that"}}]}}]}`,
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":{"prompt":"I cannot help with that"}}]}]}`,
	} {
		require.Nil(t, DetectUpstreamRejectionHTTP(rejectionTestAccount(), 200, []byte(body)))
	}
	for _, body := range []string{
		`{"type":"response.output_text.delta","output_index":0,"delta":{"prompt":"I cannot help with that"}}`,
		`{"type":"response.output_text.done","output_index":0,"text":{"prompt":"I cannot help with that"}}`,
		`{"type":"response.content_part.done","output_index":0,"part":{"type":"output_text","text":{"prompt":"I cannot help with that"}}}`,
	} {
		o := NewUpstreamRejectionObserver(rejectionTestAccount(), 200)
		o.ObserveSSEData([]byte(body))
		require.Nil(t, o.Evidence())
	}
}
