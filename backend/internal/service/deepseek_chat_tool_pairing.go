package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
)

type deepSeekToolHistoryMessage struct {
	span      deepSeekChatSpan
	pairedEnd int // end of an intact call and result block, or zero
	calls     []deepSeekToolHistoryCall
	tool      bool
	resultID  string
	content   deepSeekChatSpan
}

type deepSeekToolHistoryCall struct {
	id            string
	rawID         deepSeekChatSpan
	owner         int
	reused        bool // an earlier assistant message used this ID
	adjacentCount int  // candidates in the tool block immediately after the call
	result        *deepSeekToolHistoryMessage
	alternatives  []*deepSeekToolHistoryMessage
}

// normalizeDeepSeekChatToolPairing repairs tool message adjacency using only
// the top-level messages array. Each retained message stays byte-for-byte as
// supplied, including arguments, content and reasoning_content.
func normalizeDeepSeekChatToolPairing(ctx context.Context, body []byte) ([]byte, error) {
	top, err := scanDeepSeekChatEnvelope(ctx, body)
	if err != nil {
		return nil, err
	}
	return repairDeepSeekChatToolPairing(ctx, body, top)
}

// normalizeDeepSeekChatToolPairingForRequest prefers the fields the HTTP
// ingress already located, so an unchanged preread body is not walked again
// on its way out. A rewritten body has no index and is scanned here.
func normalizeDeepSeekChatToolPairingForRequest(ctx context.Context, req *http.Request, body []byte) ([]byte, error) {
	top, err := deepSeekChatEnvelopeForRequest(ctx, req, body)
	if err != nil {
		return nil, err
	}
	return repairDeepSeekChatToolPairing(ctx, body, top)
}

func repairDeepSeekChatToolPairing(ctx context.Context, body []byte, top deepSeekChatEnvelope) ([]byte, error) {
	if top.messages == nil || !top.messages.nonemptyArray {
		return body, nil
	}
	span := top.messages.value
	messages, err := scanDeepSeekToolHistory(ctx, body, span)
	if err != nil {
		return nil, err
	}

	// A result belongs to the most recent call with the same ID. A result
	// before the first call can only be matched later if that ID is unique.
	latestByID := make(map[string]*deepSeekToolHistoryCall)
	beforeFirstCall := make(map[string]*deepSeekToolHistoryMessage)
	competingIDs := make(map[string]bool)
	adjacentOwner := -1
	missingResults := 0
	changed := false
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		message := &messages[i]
		if !message.tool {
			adjacentOwner = i
			for j := range message.calls {
				call := &message.calls[j]
				previous := latestByID[call.id]
				if call.id == "" || (previous != nil && previous.owner == i) {
					// Unusable IDs inside a call batch cannot be repaired by pairing.
					return body, nil
				}
				call.reused = previous != nil
				latestByID[call.id] = call
				missingResults++
			}
			continue
		}

		call := latestByID[message.resultID]
		if call != nil && call.owner == adjacentOwner {
			call.adjacentCount++
		}
		switch {
		case call == nil:
			changed = true
			if message.resultID != "" && beforeFirstCall[message.resultID] == nil {
				beforeFirstCall[message.resultID] = message
			}
		case call.result == nil:
			call.result = message
			missingResults--
			changed = changed || call.adjacentCount == 0
		default:
			changed = true
			call.alternatives = append(call.alternatives, message)
			if call.reused {
				competingIDs[call.id] = true
			}
		}
	}
	if !changed && missingResults == 0 {
		return body, ctx.Err()
	}

	seenCandidates := make(map[deepSeekToolResultIdentity]struct{})
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		message := &messages[i]
		// A complete parallel block is already a coherent set of results.
		// Protect it before resolving any surplus results for reused IDs.
		if len(message.calls) > 1 {
			message.pairedEnd = deepSeekPairedBlockEnd(messages, i)
		}
		for j := range message.calls {
			call := &message.calls[j]
			if !call.reused && latestByID[call.id] == call && call.adjacentCount == 0 {
				if early := beforeFirstCall[call.id]; early != nil {
					call.result = early
				}
			}
			if call.result != nil && competingIDs[call.id] {
				if err := call.preferNewResult(ctx, body, seenCandidates, message.pairedEnd == 0); err != nil {
					return nil, err
				}
			}
		}
		if message.pairedEnd == 0 {
			message.pairedEnd = deepSeekPairedBlockEnd(messages, i)
		}
	}
	return writeDeepSeekToolHistory(ctx, body, span, messages)
}

// Each distinct call must own a result in the next len(calls) positions.
// When those positions are full, keep the input order and separators.
func deepSeekPairedBlockEnd(messages []deepSeekToolHistoryMessage, i int) int {
	message := &messages[i]
	last := i + len(message.calls)
	if last == i || last >= len(messages) {
		return 0
	}
	end := messages[last].span.end
	for _, call := range message.calls {
		if call.result == nil || call.result.span.start < message.span.end || call.result.span.end > end {
			return 0
		}
	}
	return end
}

type deepSeekToolResultIdentity struct {
	id   string
	text bool
	sum  [sha256.Size]byte
}

func deepSeekToolResultKey(body []byte, result *deepSeekToolHistoryMessage) deepSeekToolResultIdentity {
	content := body[result.content.start:result.content.end]
	key := deepSeekToolResultIdentity{id: result.resultID}
	if len(content) >= 2 && content[0] == '"' {
		key.text = true
		if bytes.IndexByte(content, '\\') >= 0 {
			key.sum = sha256.Sum256([]byte(deepSeekToolJSONString(content)))
			return key
		}
		content = content[1 : len(content)-1]
	}
	key.sum = sha256.Sum256(content)
	return key
}

// Adjacent candidates come first and take priority. Within that group, or
// among displaced candidates if there are no adjacent ones, prefer the first
// result with unseen content when replacement is allowed. Record all
// candidates for later occurrences, including those in protected blocks.
func (call *deepSeekToolHistoryCall) preferNewResult(ctx context.Context, body []byte, seenCandidates map[deepSeekToolResultIdentity]struct{}, allowReplacement bool) error {
	firstKey := deepSeekToolResultKey(body, call.result)
	_, preferredSeen := seenCandidates[firstKey]
	seenCandidates[firstKey] = struct{}{}
	for i, candidate := range call.alternatives {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := deepSeekToolResultKey(body, candidate)
		samePlacement := call.adjacentCount == 0 || i+1 < call.adjacentCount
		if _, seen := seenCandidates[key]; allowReplacement && samePlacement && preferredSeen && !seen {
			call.result = candidate
			preferredSeen = false
		}
		seenCandidates[key] = struct{}{}
	}
	return nil
}

func scanDeepSeekToolHistory(ctx context.Context, body []byte, span deepSeekChatSpan) ([]deepSeekToolHistoryMessage, error) {
	var messages []deepSeekToolHistoryMessage
	s := deepSeekChatCursor{ctx: ctx, body: body[:span.end], pos: span.start + 1}
	err := s.scanDeepSeekChatItems(']', func() error {
		var role string
		var calls, resultID, content deepSeekChatSpan
		message := deepSeekToolHistoryMessage{span: deepSeekChatSpan{start: s.pos}}
		if err := s.scanDeepSeekChatObject(func(name string, value deepSeekChatSpan) {
			switch name {
			case "role":
				role = deepSeekToolJSONString(body[value.start:value.end])
			case "tool_calls":
				calls = value
			case "tool_call_id":
				resultID = value
			case "content":
				content = value
			}
		}); err != nil {
			return err
		}
		message.span.end = s.pos
		switch role {
		case "tool":
			message.tool = true
			message.content = content
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
		if message.tool {
			continue
		}
		if message.pairedEnd != 0 {
			capacity += message.pairedEnd - message.span.start + 1
			continue
		}
		capacity += message.span.end - message.span.start + 1
		for _, call := range message.calls {
			if call.result != nil {
				capacity += call.result.span.end - call.result.span.start + 1
			} else {
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
		if message.pairedEnd != 0 {
			if err := appendMessage(deepSeekChatSpan{start: message.span.start, end: message.pairedEnd}); err != nil {
				return nil, err
			}
			continue
		}
		if err := appendMessage(message.span); err != nil {
			return nil, err
		}
		for _, call := range message.calls {
			if call.result != nil {
				if err := appendMessage(call.result.span); err != nil {
					return nil, err
				}
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
