package requestmodel

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
)

// ChatBodyField holds the offsets of one top-level field in a preread body.
// A zero ValueStart means the field was absent.
type ChatBodyField struct {
	ValueStart, ValueEnd           int
	KeyStart, PrevComma, NextComma int
	Index                          int
	NonemptyArray                  bool
}

// ChatBodyFields is the part of the JSON envelope needed by the Chat tool filter.
// These offsets describe the original bytes; syntax validation is still required.
type ChatBodyFields struct {
	Tools, ToolChoice, Messages, Input ChatBodyField
	Close, Count                       int
}

type indexedChatBody struct {
	*httputil.PrereadBody
	fields ChatBodyFields
}

// ResetAndCheckChatBody checks for duplicate fields and indexes the top-level
// Chat fields in the same scan. The index lives with the immutable preread
// body, so replacing that body invalidates it, and it does not enter the context.
func ResetAndCheckChatBody(req *http.Request, body []byte) (string, bool) {
	var scan chatBodyScanner
	// ConflictingField strips a BOM internally; only attach offsets when they
	// describe the exact slice being returned to the handler.
	var index *chatBodyScanner
	if len(TrimUTF8BOM(body)) == len(body) {
		index = &scan
	}
	field, conflict := conflictingField(req.Header.Get("Content-Type"), body, index)
	var fields *ChatBodyFields
	if !conflict && scan.complete {
		fields = &scan.fields
	}
	resetPrereadBody(req, body, fields)
	return field, conflict
}

// ChatFieldsForBody returns the index only for the original immutable slice.
// Callers forwarding a copy or a rewritten body must inspect that body instead.
func ChatFieldsForBody(req *http.Request, body []byte) *ChatBodyFields {
	if req == nil || len(body) == 0 {
		return nil
	}
	indexed, ok := req.Body.(*indexedChatBody)
	if !ok {
		return nil
	}
	original := indexed.Bytes()
	if len(original) != len(body) || &original[0] != &body[0] {
		return nil
	}
	return &indexed.fields
}

type chatBodyScanner struct {
	fields   ChatBodyFields
	current  *ChatBodyField
	comma    int
	invalid  bool
	complete bool
}

func (s *chatBodyScanner) member(body, name []byte, keyStart, afterKey int) int {
	index := s.fields.Count
	s.fields.Count++
	var field *ChatBodyField
	switch string(name) {
	case "tools":
		field = &s.fields.Tools
	case "tool_choice":
		field = &s.fields.ToolChoice
	case "messages":
		field = &s.fields.Messages
	case "input":
		field = &s.fields.Input
	default:
		return afterKey
	}
	colon := skipChatJSONSpace(body, afterKey)
	if colon >= len(body) || body[colon] != ':' {
		s.invalid = true
		return afterKey
	}
	start := skipChatJSONSpace(body, colon+1)
	prevComma := -1
	if index > 0 {
		prevComma = s.comma
	}
	*field = ChatBodyField{ValueStart: start, KeyStart: keyStart, Index: index, PrevComma: prevComma, NextComma: -1}
	s.current = field
	return start
}

func (s *chatBodyScanner) delimiter(body []byte, at int, comma bool) {
	if field := s.current; field != nil {
		end := at
		for end > field.ValueStart && chatJSONSpace(body[end-1]) {
			end--
		}
		if end <= field.ValueStart {
			s.invalid = true
		} else {
			field.ValueEnd = end
			if body[field.ValueStart] == '[' {
				field.NonemptyArray = skipChatJSONSpace(body[:end], field.ValueStart+1) < end-1
			}
			if comma {
				field.NextComma = at
			}
		}
		s.current = nil
	}
	if comma {
		s.comma = at
	} else {
		s.fields.Close = at
		s.complete = !s.invalid && skipChatJSONSpace(body, at+1) == len(body)
	}
}

func skipChatJSONSpace(body []byte, from int) int {
	for from < len(body) && chatJSONSpace(body[from]) {
		from++
	}
	return from
}

func chatJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
