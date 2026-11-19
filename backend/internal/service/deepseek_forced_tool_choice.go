package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeDeepSeekForcedToolChoice turns thinking off for the one request in
// which the client forces a tool call.
//
// DeepSeek thinks by default and rejects a forced tool_choice ("required" or a
// named function) while thinking is on: HTTP 400 "Thinking mode does not
// support this tool_choice". Agent frameworks force a tool to get structured
// output, so every such request fails. Downgrading the choice to "auto" would
// keep thinking but lets the model answer in prose, which breaks those callers
// in a different way; turning thinking off keeps the tool call that was asked
// for. An explicit thinking request loses to the forced choice for the same
// reason: the upstream cannot honor both.
//
// Only a forced request to a DeepSeek upstream is rewritten. "auto", "none", a
// missing tool_choice and every other upstream pass through byte for byte.
func normalizeDeepSeekForcedToolChoice(account *Account, body []byte) []byte {
	if !targetsDeepSeekAPIHost(account) || !deepSeekChatToolChoiceIsForced(gjson.GetBytes(body, "tool_choice")) {
		return body
	}
	hasEffort := gjson.GetBytes(body, "reasoning_effort").Exists()
	if gjson.GetBytes(body, "thinking.type").String() == "disabled" && !hasEffort {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"disabled"}`))
	if err != nil {
		return body
	}
	if hasEffort {
		// An effort level asks for thinking; do not send it next to "disabled".
		if updated, err = sjson.DeleteBytes(updated, "reasoning_effort"); err != nil {
			return body
		}
	}
	return updated
}

// deepSeekChatToolChoiceIsForced reports whether a Chat Completions tool_choice
// obliges the model to call a tool.
func deepSeekChatToolChoiceIsForced(choice gjson.Result) bool {
	switch {
	case choice.Type == gjson.String:
		return strings.EqualFold(strings.TrimSpace(choice.String()), "required")
	case choice.IsObject():
		return strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "function")
	}
	return false
}
