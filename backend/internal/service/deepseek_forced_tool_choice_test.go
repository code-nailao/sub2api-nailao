package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const deepSeekForcedToolChoiceTools = `"tools":[{"type":"function","function":{"name":"record","parameters":{"type":"object","properties":{}}}}]`

func TestNormalizeDeepSeekForcedToolChoice(t *testing.T) {
	deepSeek := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	officialHost := openaiPlatformDeepSeekAccount()
	other := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "http://upstream.example"},
	}
	body := func(extra string) []byte {
		return []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],` + deepSeekForcedToolChoiceTools + extra + `}`)
	}

	forced := map[string]string{
		"required":                 `,"tool_choice":"required"`,
		"named_function":           `,"tool_choice":{"type":"function","function":{"name":"record"}}`,
		"required_with_effort":     `,"tool_choice":"required","reasoning_effort":"high"`,
		"required_thinking_on":     `,"tool_choice":"required","thinking":{"type":"enabled"}`,
		"disabled_but_with_effort": `,"tool_choice":"required","thinking":{"type":"disabled"},"reasoning_effort":"max"`,
	}
	for name, extra := range forced {
		t.Run("forced_"+name, func(t *testing.T) {
			in := body(extra)
			for _, account := range []*Account{deepSeek, officialHost} {
				got := normalizeDeepSeekForcedToolChoice(account, in)
				require.True(t, gjson.ValidBytes(got))
				require.Equal(t, "disabled", gjson.GetBytes(got, "thinking.type").String())
				require.False(t, gjson.GetBytes(got, "reasoning_effort").Exists())
				// The client's choice, tools and messages are what it sent.
				require.Equal(t, gjson.GetBytes(in, "tool_choice").Raw, gjson.GetBytes(got, "tool_choice").Raw)
				require.Equal(t, gjson.GetBytes(in, "tools").Raw, gjson.GetBytes(got, "tools").Raw)
				require.Equal(t, gjson.GetBytes(in, "messages").Raw, gjson.GetBytes(got, "messages").Raw)
			}
		})
	}

	untouched := map[string]string{
		"no_tool_choice":         ``,
		"auto":                   `,"tool_choice":"auto"`,
		"auto_with_effort":       `,"tool_choice":"auto","reasoning_effort":"high","thinking":{"type":"enabled"}`,
		"none":                   `,"tool_choice":"none"`,
		"null":                   `,"tool_choice":null`,
		"object_auto":            `,"tool_choice":{"type":"auto"}`,
		"already_disabled":       `,"tool_choice":"required","thinking":{"type":"disabled"}`,
		"unrelated_string_value": `,"tool_choice":"record"`,
	}
	for name, extra := range untouched {
		t.Run("untouched_"+name, func(t *testing.T) {
			in := body(extra)
			require.Equal(t, string(in), string(normalizeDeepSeekForcedToolChoice(deepSeek, in)))
		})
	}

	t.Run("other_upstream_untouched", func(t *testing.T) {
		in := body(`,"tool_choice":"required","reasoning_effort":"high"`)
		require.Equal(t, string(in), string(normalizeDeepSeekForcedToolChoice(other, in)))
		require.Equal(t, string(in), string(normalizeDeepSeekForcedToolChoice(nil, in)))
	})

	t.Run("invalid_body_untouched", func(t *testing.T) {
		in := []byte(`not json`)
		require.Equal(t, string(in), string(normalizeDeepSeekForcedToolChoice(deepSeek, in)))
	})
}

func newDeepSeekForcedToolChoiceContext(t *testing.T, path string, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func deepSeekForcedToolChoiceAccount() *Account {
	return &Account{
		ID:          31,
		Name:        "deepseek-apikey",
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://provider.example/v1", "api_protocol": "chat_completions"},
		Status:      StatusActive,
		Schedulable: true,
	}
}

// All three inbound protocols reach a Chat Completions DeepSeek upstream
// through the same outbound point, so each must arrive with thinking off.
func TestDeepSeekForcedToolChoiceDisablesThinkingOnEveryInboundProtocol(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		forward func(*OpenAIGatewayService, *gin.Context, *Account, []byte) (*OpenAIForwardResult, error)
	}{
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			body: `{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],` + deepSeekForcedToolChoiceTools + `,"tool_choice":"required","reasoning_effort":"high"}`,
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, a, b, "", "")
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"deepseek-flash","input":"hi","tools":[{"type":"function","name":"record","parameters":{"type":"object","properties":{}}}],"tool_choice":"required","reasoning":{"effort":"high"}}`,
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.Forward(context.Background(), c, a, b)
			},
		},
		{
			name: "messages",
			path: "/v1/messages",
			body: `{"model":"deepseek-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"record","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"tool","name":"record"}}`,
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.ForwardAsAnthropic(context.Background(), c, a, b, "", "")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			c := newDeepSeekForcedToolChoiceContext(t, tc.path, body)
			upstream := newOKChatCompletionsUpstream("rid_ds_forced_choice", deepSeekChatFallbackOKBody)
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}

			result, err := tc.forward(svc, c, deepSeekForcedToolChoiceAccount(), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, upstream.lastReq.URL.String(), "/chat/completions")
			require.True(t, deepSeekChatToolChoiceIsForced(gjson.GetBytes(upstream.lastBody, "tool_choice")), "upstream body: %s", upstream.lastBody)
			require.Equal(t, "disabled", gjson.GetBytes(upstream.lastBody, "thinking.type").String(), "upstream body: %s", upstream.lastBody)
			require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
		})
	}
}

func TestDeepSeekAutoToolChoiceKeepsThinking(t *testing.T) {
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],` + deepSeekForcedToolChoiceTools + `,"tool_choice":"auto","reasoning_effort":"high"}`)
	c := newDeepSeekForcedToolChoiceContext(t, "/v1/chat/completions", body)
	upstream := newOKChatCompletionsUpstream("rid_ds_auto_choice", deepSeekChatFallbackOKBody)
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, deepSeekForcedToolChoiceAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, gjson.GetBytes(upstream.lastBody, "thinking").Exists())
	require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
}
