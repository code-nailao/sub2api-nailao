package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Only terminal server_tool_use counters are successful units. Output items,
// query arrays and client-side tool discovery never determine a charge.
func deepSeekNativeTerminalWithType(body []byte, eventType string) (gjson.Result, bool) {
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, false
	}
	root := gjson.ParseBytes(body)
	typ := root.Get("type").String()
	if typ == "" {
		typ = eventType
	}
	if typ != "" {
		if typ != "response.completed" && typ != "response.done" && typ != "response.incomplete" && typ != "response.failed" {
			return gjson.Result{}, false
		}
		root = root.Get("response")
	}
	if status := root.Get("status"); status.Exists() && status.String() != "completed" && status.String() != "incomplete" && status.String() != "failed" {
		return gjson.Result{}, false
	}
	return root, root.IsObject()
}

func deepSeekNativeCountsFromResponse(root gjson.Result) (search, fetch int, ok bool) {
	counts := root.Get("usage.server_tool_use")
	if !counts.IsObject() {
		return
	}
	read := func(k string) (int, bool) {
		n := counts.Get(k)
		if !n.Exists() {
			return 0, true
		}
		if n.Type != gjson.Number || n.Float() < 0 || n.Float() > 1000000 || n.Float() != float64(n.Int()) {
			return 0, false
		}
		return int(n.Int()), true
	}
	var a, b bool
	search, a = read("tool_search")
	fetch, b = read("tool_fetch")
	ok = a && b
	return
}

func (w *deepSeekCapture) observeNativeTerminal(body []byte) {
	w.observeNativeTerminalWithType(body, "")
}

func (w *deepSeekCapture) observeNativeTerminalWithType(body []byte, eventType string) {
	root, terminal := deepSeekNativeTerminalWithType(body, eventType)
	if !terminal {
		return
	}
	if usage, ok := extractOpenAIUsageFromJSONBytes([]byte(root.Raw)); ok {
		w.nativeUsage = usage
		w.nativeUsageObserved = true
	}
	if search, fetch, ok := deepSeekNativeCountsFromResponse(root); ok {
		w.nativeSearch = search
		w.nativeFetch = fetch
		w.nativeObserved = true
	}
}

// Observe the provider's terminal before the shared forwarder sanitizes failed
// responses or returns early. Client-visible bytes need not retain billing data.
func observeDeepSeekNativeSSE(c *gin.Context, body []byte, eventType string) {
	if !c.GetBool(deepSeekInternalTurn) {
		return
	}
	capture, ok := c.Writer.(*deepSeekCapture)
	if !ok || !capture.tee {
		return
	}
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		capture.observeNativeTerminalWithType(body, eventType)
	}
}

func (w *deepSeekCapture) observeNative(b []byte) {
	if !w.tee {
		return
	}
	w.nativeLine = append(w.nativeLine, b...)
	for {
		i := bytes.IndexByte(w.nativeLine, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(w.nativeLine[:i])
		w.nativeLine = w.nativeLine[i+1:]
		if bytes.HasPrefix(line, []byte("data:")) {
			w.observeNativeTerminal(bytes.TrimSpace(line[5:]))
		}
	}
	if len(w.nativeLine) > deepSeekToolResponseLimit {
		w.nativeLine = nil
	}
}

func deepSeekCanUseNative(account, supplier *Account, body []byte, protocol string) bool {
	if protocol != "responses" || account.ID != supplier.ID || !account.UsesNativeCNResponses() || account.Extra["deepseek_native_tools_enabled"] != true {
		return false
	}
	if override, _ := account.Extra["deepseek_tools_base_url"].(string); strings.TrimSpace(override) != "" {
		return false
	}
	// Enforced request budgets belong to the hosted executor; native providers
	// may ignore max_tool_calls and do not expose per-operation admission.
	if gjson.GetBytes(body, "max_tool_calls").Exists() {
		return false
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("max_uses").Exists() {
			return false
		}
	}
	model := strings.ToLower(account.GetMappedModel(gjson.GetBytes(body, "model").String()))
	return strings.HasPrefix(model, "deepseek-") && slices.Contains(strings.Split(model, "-"), "pro")
}

func (s *OpenAIGatewayService) forwardDeepSeekNative(ctx context.Context, c *gin.Context, account *Account, body []byte, key *APIKey, usage *DeepSeekToolUsage) (*OpenAIForwardResult, error) {
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	tools, _ := b["tools"].([]any)
	hasServerTool := false
	for _, v := range tools {
		t, _ := v.(map[string]any)
		typ, _ := t["type"].(string)
		if deepSeekExplicitTool(typ) != "" {
			hasServerTool = true
		}
	}
	if !hasServerTool && key.DeepSeekToolsAutoInject() {
		tools = append(tools, map[string]any{"type": "web_search"})
		b["tools"] = tools
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	parent := &OpenAIForwardResult{RequestID: "deepseek-tools:" + uuid.NewString(), Model: gjson.GetBytes(body, "model").String(), Stream: gjson.GetBytes(body, "stream").Bool(), DeepSeekTools: usage}
	if err = s.beginDeepSeekHostedUsage(ctx, c, key, account, parent); err != nil {
		return nil, err
	}
	c.Header("X-Sources-Request-Id", parent.RequestID)
	nativeCtx, cancel := context.WithTimeout(ctx, deepSeekRequestTimeout)
	defer cancel()
	started := time.Now()
	result, capture, err := s.deepSeekModelTurn(nativeCtx, c, account, payload, "responses", "", true)
	capture.observeNativeTerminal(capture.body.Bytes())
	if result == nil && (capture.nativeUsageObserved || capture.nativeObserved) {
		// The underlying forwarder may discard its result on a terminal stream
		// error. Preserve only usage actually observed on that terminal so the
		// caller settles confirmed work instead of replaying it on failover.
		billingModel, upstreamModel := resolveOpenAIForwardMappedModels(account, parent.Model, isOpenAIResponsesCompactPath(c))
		result = &OpenAIForwardResult{
			Usage:         capture.nativeUsage,
			Model:         parent.Model,
			BillingModel:  billingModel,
			UpstreamModel: upstreamModel,
			Stream:        parent.Stream,
		}
		stampOpenAIResponsesUpstreamEndpoint(c, result)
	}
	if result != nil {
		copy := *usage
		if capture.nativeObserved {
			copy.SearchCount = capture.nativeSearch
			copy.FetchCount = capture.nativeFetch
		}
		result.DeepSeekTools = &copy
		result.RequestID = parent.RequestID
		result.deepSeekBilling = parent.deepSeekBilling
		result.Duration = time.Since(started)
		if journalErr := s.checkpointDeepSeekUsage(ctx, result); journalErr != nil {
			return result, journalErr
		}
	} else if err == nil {
		err = errors.New("DeepSeek native response has no confirmed usage")
	}
	return result, err
}
