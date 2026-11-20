package service

import (
	"context"
	"encoding/json"
)

type deepSeekToolHistoryMessage struct {
	span     deepSeekChatSpan
	calls    []deepSeekToolHistoryCall
	results  []deepSeekChatSpan
	tool     bool
	resultID string
	owner    int // message index of the assistant call, or -1
}

type deepSeekToolHistoryCall struct {
	id     string
	rawID  deepSeekChatSpan
	owner  int
	paired bool
}

// normalizeDeepSeekChatToolPairing repairs tool message adjacency using only
// the top-level messages array. Each retained message stays byte-for-byte as
// supplied, including arguments, content and reasoning_content.
func normalizeDeepSeekChatToolPairing(ctx context.Context, body []byte) ([]byte, error) {
	top, err := scanDeepSeekChatEnvelope(ctx, body)
	if err != nil {
		return nil, err
	}
	if top.messages == nil || !top.messages.nonemptyArray {
		return body, nil
	}
	span := top.messages.value
	messages, err := scanDeepSeekToolHistory(ctx, body, span)
	if err != nil {
		return nil, err
	}

	// Preserve existing adjacent matches first, even if different turns reused
	// an ID.
	adjacent := make(map[string]*deepSeekToolHistoryCall)
	changed := false
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		message := &messages[i]
		if message.tool {
			if call := adjacent[message.resultID]; call != nil {
				message.owner, call.paired = call.owner, true
				delete(adjacent, call.id)
			} else {
				changed = true
			}
			continue
		}
		clear(adjacent)
		for j := range message.calls {
			call := &message.calls[j]
			if call.id == "" || adjacent[call.id] != nil {
				// Unusable IDs inside a call batch cannot be repaired by pairing.
				return body, nil
			}
			adjacent[call.id] = call
		}
	}

	// Only calls still missing a result can own a displaced result. Multiple
	// unmatched calls with the same ID leave that result ambiguous.
	unpairedByID := make(map[string]*deepSeekToolHistoryCall)
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for j := range messages[i].calls {
			call := &messages[i].calls[j]
			if call.paired {
				continue
			}
			if _, exists := unpairedByID[call.id]; exists {
				unpairedByID[call.id] = nil
			} else {
				unpairedByID[call.id] = call
			}
		}
	}
	if !changed && len(unpairedByID) == 0 {
		return body, ctx.Err()
	}
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		message := &messages[i]
		if !message.tool {
			continue
		}
		if message.owner < 0 {
			if call := unpairedByID[message.resultID]; call != nil && !call.paired {
				message.owner, call.paired = call.owner, true
			}
		}
		if message.owner >= 0 {
			owner := &messages[message.owner]
			owner.results = append(owner.results, message.span)
		}
	}
	return writeDeepSeekToolHistory(ctx, body, span, messages)
}

func scanDeepSeekToolHistory(ctx context.Context, body []byte, span deepSeekChatSpan) ([]deepSeekToolHistoryMessage, error) {
	var messages []deepSeekToolHistoryMessage
	s := deepSeekChatCursor{ctx: ctx, body: body[:span.end], pos: span.start + 1}
	err := s.scanDeepSeekChatItems(']', func() error {
		var role string
		var calls, resultID deepSeekChatSpan
		message := deepSeekToolHistoryMessage{span: deepSeekChatSpan{start: s.pos}, owner: -1}
		if err := s.scanDeepSeekChatObject(func(name string, value deepSeekChatSpan) {
			switch name {
			case "role":
				role = deepSeekToolJSONString(body[value.start:value.end])
			case "tool_calls":
				calls = value
			case "tool_call_id":
				resultID = value
			}
		}); err != nil {
			return err
		}
		message.span.end = s.pos
		switch role {
		case "tool":
			message.tool = true
			message.resultID = deepSeekToolJSONString(body[resultID.start:resultID.end])
		case "assistant":
			if calls.end > calls.start && body[calls.start] == '[' {
				c := deepSeekChatCursor{ctx: ctx, body: body[:calls.end], pos: calls.start + 1}
				if err := c.scanDeepSeekChatItems(']', func() error {
					call := deepSeekToolHistoryCall{owner: len(messages)}
					if err := c.scanDeepSeekChatObject(func(name string, value deepSeekChatSpan) {
						if name == "id" {
							call.rawID = value
						}
					}); err != nil {
						return err
					}
					call.id = deepSeekToolJSONString(body[call.rawID.start:call.rawID.end])
					message.calls = append(message.calls, call)
					return nil
				}); err != nil {
					return err
				}
			}
		}
		messages = append(messages, message)
		return nil
	})
	return messages, err
}

// Only decode the fields used for pairing. Other values are skipped in place.
func (s *deepSeekChatCursor) scanDeepSeekChatObject(field func(string, deepSeekChatSpan)) error {
	if !s.take('{') {
		_, err := s.skipValue()
		return err
	}
	return s.scanDeepSeekChatItems('}', func() error {
		key := s.pos
		if s.peek() != '"' {
			return s.invalid()
		}
		if err := s.skipString(); err != nil {
			return err
		}
		name := deepSeekChatFieldName(s.body[key:s.pos])
		s.skipSpace()
		if !s.take(':') {
			return s.invalid()
		}
		s.skipSpace()
		start := s.pos
		if _, err := s.skipValue(); err != nil {
			return err
		}
		field(name, deepSeekChatSpan{start, s.pos})
		return nil
	})
}

func (s *deepSeekChatCursor) scanDeepSeekChatItems(end byte, item func() error) error {
	s.skipSpace()
	if s.take(end) {
		return nil
	}
	for {
		if err := item(); err != nil {
			return err
		}
		s.skipSpace()
		if s.take(end) {
			return nil
		}
		if !s.take(',') {
			return s.invalid()
		}
		s.skipSpace()
	}
}

func deepSeekToolJSONString(raw []byte) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func writeDeepSeekToolHistory(ctx context.Context, body []byte, span deepSeekChatSpan, messages []deepSeekToolHistoryMessage) ([]byte, error) {
	const prefix = `{"role":"tool","tool_call_id":`
	const suffix = `,"content":""}`
	capacity := len(body) - (span.end - span.start) + 2
	for _, message := range messages {
		if !message.tool || message.owner >= 0 {
			capacity += message.span.end - message.span.start + 1
		}
		for _, call := range message.calls {
			if !call.paired {
				capacity += len(prefix) + call.rawID.end - call.rawID.start + len(suffix) + 1
			}
		}
	}
	out, err := appendDeepSeekChatBytes(ctx, make([]byte, 0, capacity), body[:span.start])
	if err != nil {
		return nil, err
	}
	out = append(out, '[')
	first := true
	separator := func() {
		if !first {
			out = append(out, ',')
		}
		first = false
	}
	appendMessage := func(raw deepSeekChatSpan) error {
		separator()
		out, err = appendDeepSeekChatBytes(ctx, out, body[raw.start:raw.end])
		return err
	}
	for _, message := range messages {
		if message.tool {
			continue
		}
		if err := appendMessage(message.span); err != nil {
			return nil, err
		}
		for _, result := range message.results {
			if err := appendMessage(result); err != nil {
				return nil, err
			}
		}
		for _, call := range message.calls {
			if call.paired {
				continue
			}
			separator()
			out = append(out, prefix...)
			out, err = appendDeepSeekChatBytes(ctx, out, body[call.rawID.start:call.rawID.end])
			if err != nil {
				return nil, err
			}
			out = append(out, suffix...)
		}
	}
	out = append(out, ']')
	return appendDeepSeekChatBytes(ctx, out, body[span.end:])
}
