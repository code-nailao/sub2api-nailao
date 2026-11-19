package service

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

func deepSeekToolEvent(call deepSeekOwnedCall, data []byte, success bool) map[string]any {
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	var payload any
	_ = json.Unmarshal(data, &payload)
	status := "failed"
	if success {
		status = "completed"
	}
	action := map[string]any{"type": "search", "query": args["query"]}
	if call.Operation == "fetch" {
		action = map[string]any{"type": "open_page", "url": args["url"]}
	} else {
		sources := []map[string]any{}
		for _, r := range gjson.GetBytes(data, "results").Array() {
			sources = append(sources, map[string]any{"type": "url", "url": r.Get("url").String(), "title": r.Get("title").String()})
		}
		action["sources"] = sources
	}
	return map[string]any{"type": "web_search_call", "id": "ws_" + call.ID, "status": status, "action": action, "sources_operation": call.Operation, "sources_input": args, "sources_result": payload}
}

func deepSeekPublicEvent(e map[string]any) map[string]any {
	return map[string]any{"type": e["type"], "id": e["id"], "status": e["status"], "action": e["action"]}
}

func finishDeepSeekHostedResponse(protocol string, body []byte, result *OpenAIForwardResult, events []map[string]any) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	usage, _ := b["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	u := result.Usage
	switch protocol {
	case "chat":
		usage["prompt_tokens"] = u.InputTokens
		usage["completion_tokens"] = u.OutputTokens
		usage["total_tokens"] = u.InputTokens + u.OutputTokens
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens, "cache_creation_tokens": u.CacheCreationInputTokens}
	case "messages":
		usage["input_tokens"] = max(0, u.InputTokens-u.CacheReadInputTokens-u.CacheCreationInputTokens)
		usage["output_tokens"] = u.OutputTokens
		usage["cache_read_input_tokens"] = u.CacheReadInputTokens
		usage["cache_creation_input_tokens"] = u.CacheCreationInputTokens
	default:
		usage["input_tokens"] = u.InputTokens
		usage["output_tokens"] = u.OutputTokens
		usage["total_tokens"] = u.InputTokens + u.OutputTokens
		usage["input_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens, "cache_creation_tokens": u.CacheCreationInputTokens}
	}
	usage["server_tool_use"] = map[string]int{"tool_search": result.DeepSeekTools.SearchCount, "tool_fetch": result.DeepSeekTools.FetchCount}
	b["usage"] = usage
	switch protocol {
	case "messages":
		content := []any{}
		for _, e := range events {
			op, _ := e["sources_operation"].(string)
			id := "srvtoolu_" + fmt.Sprint(e["id"])
			content = append(content, map[string]any{"type": "server_tool_use", "id": id, "name": "web_" + op, "input": e["sources_input"]})
			payload, _ := e["sources_result"].(map[string]any)
			var toolContent any
			if e["status"] != "completed" {
				toolContent = map[string]any{"type": "web_" + op + "_tool_result_error", "error_code": "unavailable"}
			} else if op == "search" {
				rows := []any{}
				if entries, ok := payload["results"].([]any); ok {
					for _, entry := range entries {
						r, _ := entry.(map[string]any)
						rows = append(rows, map[string]any{"type": "web_search_result", "url": r["url"], "title": r["title"], "encrypted_content": ""})
					}
				}
				toolContent = rows
			} else {
				input, ok := e["sources_input"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid DeepSeek fetch input")
				}
				toolContent = map[string]any{"type": "web_fetch_result", "url": input["url"], "content": map[string]any{"type": "document", "title": payload["title"], "source": map[string]any{"type": "text", "media_type": "text/plain", "data": payload["content"]}}}
			}
			content = append(content, map[string]any{"type": "web_" + op + "_tool_result", "tool_use_id": id, "content": toolContent})
		}
		existing, _ := b["content"].([]any)
		b["content"] = append(content, existing...)
	case "chat":
		calls := []any{}
		for _, e := range events {
			calls = append(calls, deepSeekPublicEvent(e))
		}
		if len(calls) > 0 {
			b["server_tool_calls"] = calls
		}
	default:
		items := []any{}
		for _, e := range events {
			items = append(items, deepSeekPublicEvent(e))
		}
		existing, _ := b["output"].([]any)
		b["output"] = append(items, existing...)
	}
	// Do not expose randomly named internal functions in the echoed tools field.
	delete(b, "tools")
	return json.Marshal(b)
}

func removeDeepSeekOwnedCalls(protocol string, body []byte, owned map[string]string) []byte {
	var b map[string]any
	if json.Unmarshal(body, &b) != nil {
		return body
	}
	filter := func(items []any, chat bool) []any {
		out := []any{}
		for _, v := range items {
			item, _ := v.(map[string]any)
			name, _ := item["name"].(string)
			if chat {
				fn, _ := item["function"].(map[string]any)
				name, _ = fn["name"].(string)
			}
			if _, ok := owned[name]; !ok {
				out = append(out, v)
			}
		}
		return out
	}
	switch protocol {
	case "chat":
		choices, _ := b["choices"].([]any)
		for _, v := range choices {
			choice, _ := v.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			items, _ := message["tool_calls"].([]any)
			message["tool_calls"] = filter(items, true)
		}
	case "messages":
		items, _ := b["content"].([]any)
		b["content"] = filter(items, false)
	default:
		items, _ := b["output"].([]any)
		b["output"] = filter(items, false)
	}
	out, err := json.Marshal(b)
	if err != nil {
		return body
	}
	return out
}
