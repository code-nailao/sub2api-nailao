package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	deepSeekChatJSONMaxDepth = 128 // including the request object
	deepSeekJSONChunk        = 4 << 10
	deepSeekMaxRequestTools  = 32 << 10
)

var errDeepSeekInvalidJSON = errors.New("invalid JSON in Chat request")

type deepSeekChatSpan struct{ start, end int }

type deepSeekChatField struct {
	value            deepSeekChatSpan
	keyStart, comma  int
	nextComma, index int
	nonemptyArray    bool
}

type deepSeekChatEnvelope struct {
	tools, choice, messages, input *deepSeekChatField
	duplicate                      string
	end, fields                    int
}

// deepSeekChatCursor locates top-level fields. The Chat ingress has already
// validated JSON syntax. Nested values here are opaque, except for matching
// delimiters and recognizing strings so an escaped quote cannot end a value.
// Keeping a byte budget also covers large strings, whitespace and scalars.
type deepSeekChatCursor struct {
	ctx            context.Context
	body           []byte
	pos, nextCheck int
	err            error
}

func (s *deepSeekChatCursor) peek() byte {
	if s.pos >= s.nextCheck {
		s.err = s.ctx.Err()
		s.nextCheck = s.pos + deepSeekJSONChunk
	}
	if s.err != nil || s.pos >= len(s.body) {
		return 0
	}
	return s.body[s.pos]
}

func deepSeekJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func (s *deepSeekChatCursor) skipSpace() {
	for deepSeekJSONSpace(s.peek()) {
		s.pos++
	}
}

func (s *deepSeekChatCursor) take(b byte) bool {
	if s.peek() != b {
		return false
	}
	s.pos++
	return true
}

func (s *deepSeekChatCursor) invalid() error {
	if s.err != nil {
		return s.err
	}
	return errDeepSeekInvalidJSON
}

func (s *deepSeekChatCursor) skipString() error {
	s.pos++ // opening quote
	for {
		switch s.peek() {
		case 0:
			return s.invalid()
		case '"':
			s.pos++
			return nil
		case '\\':
			s.pos++
			if s.peek() == 0 {
				return s.invalid()
			}
		}
		s.pos++
	}
}

func (s *deepSeekChatCursor) skipValue() (bool, error) {
	first := s.peek()
	switch first {
	case '"':
		return false, s.skipString()
	case '[', '{':
		// Only structural bytes affect the span. Use an iterative scan so
		// skipping unrelated values needs no decoding or recursive descent.
		s.pos++
		s.skipSpace()
		nonemptyArray := first == '[' && s.peek() != ']'
		depth := 1
		for {
			switch s.peek() {
			case 0:
				return false, s.invalid()
			case '"':
				if err := s.skipString(); err != nil {
					return false, err
				}
				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					s.pos++
					return nonemptyArray, nil
				}
			}
			s.pos++
		}
	case 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		start := s.pos
		for b := s.peek(); b != 0 && !deepSeekJSONSpace(b) && b != ',' && b != '}'; b = s.peek() {
			s.pos++
		}
		if s.pos == start || s.err != nil {
			return false, s.invalid()
		}
		return false, nil
	default:
		return false, s.invalid()
	}
}

func deepSeekChatFieldName(raw []byte) string {
	// A relevant key, even with every character Unicode-escaped, is short.
	// Avoid decoding arbitrary top-level keys that cannot match it.
	if len(raw) > 2+6*len("tool_choice") {
		return ""
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return ""
	}
	return name
}

// Scan the envelope once. Untracked fields remain in the original byte ranges
// between recorded fields, so reconstructing the body doesn't need a key map.
func scanDeepSeekChatEnvelope(ctx context.Context, body []byte) (deepSeekChatEnvelope, error) {
	var top deepSeekChatEnvelope
	s := deepSeekChatCursor{ctx: ctx, body: body}
	s.skipSpace()
	if !s.take('{') {
		return top, s.invalid()
	}
	s.skipSpace()
	comma := -1
	if s.peek() != '}' {
		for {
			keyStart := s.pos
			if s.peek() != '"' {
				return top, s.invalid()
			}
			if err := s.skipString(); err != nil {
				return top, err
			}
			name := deepSeekChatFieldName(body[keyStart:s.pos])
			s.skipSpace()
			if !s.take(':') {
				return top, s.invalid()
			}
			s.skipSpace()
			start := s.pos
			nonemptyArray, err := s.skipValue()
			if err != nil {
				return top, err
			}
			var field **deepSeekChatField
			switch name {
			case "tools":
				field = &top.tools
			case "tool_choice":
				field = &top.choice
			case "messages":
				field = &top.messages
			case "input":
				field = &top.input
			}
			var previous *deepSeekChatField
			if field != nil {
				if *field != nil && (name == "tools" || name == "tool_choice") {
					if top.duplicate == "" {
						top.duplicate = name
					}
				} else {
					// Keep the last messages/input span, as JSON decoding does.
					if *field == nil {
						*field = new(deepSeekChatField)
					}
					previous = *field
					*previous = deepSeekChatField{value: deepSeekChatSpan{start, s.pos}, keyStart: keyStart,
						comma: comma, nextComma: -1, index: top.fields, nonemptyArray: nonemptyArray}
				}
			}
			top.fields++
			s.skipSpace()
			comma = s.pos
			if !s.take(',') {
				break
			}
			if previous != nil {
				previous.nextComma = comma
			}
			s.skipSpace()
		}
	}
	top.end = s.pos
	if !s.take('}') {
		return top, s.invalid()
	}
	s.skipSpace()
	if s.pos != len(body) || s.err != nil {
		return top, s.invalid()
	}
	return top, ctx.Err()
}

// Only tool spans use the decoder. Refill checks bound the work between
// cancellation checks even while the decoder scans a large string token.
type deepSeekJSONReader struct {
	ctx  context.Context
	body []byte
}

func (r *deepSeekJSONReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), deepSeekJSONChunk)], r.body)
	r.body = r.body[n:]
	return n, nil
}

type deepSeekChatToolFields struct {
	typ, name, functionName string
}

func (f deepSeekChatToolFields) matchesKeptName(name string) bool {
	if f.typ != "function" {
		return name == f.name || name == f.typ
	}
	if f.functionName != "" {
		return name == f.functionName
	}
	// Fixed Chat accounts can receive flat function definitions in a raw
	// Responses-shaped request.
	return name == f.name
}

type deepSeekChatToolScope uint8

const (
	deepSeekChatToolNested deepSeekChatToolScope = iota
	deepSeekChatToolTop
	deepSeekChatToolFunction
)

type deepSeekChatToolParser struct {
	ctx context.Context
	dec *json.Decoder
}

func newDeepSeekChatToolParser(ctx context.Context, raw []byte) deepSeekChatToolParser {
	dec := json.NewDecoder(&deepSeekJSONReader{ctx: ctx, body: raw})
	dec.UseNumber()
	return deepSeekChatToolParser{ctx, dec}
}

func (p deepSeekChatToolParser) next() (json.Token, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	token, err := p.dec.Token()
	if err != nil {
		if ctxErr := p.ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errDeepSeekInvalidJSON
	}
	return token, nil
}

func (p deepSeekChatToolParser) parseValue(first json.Token, depth int, scope deepSeekChatToolScope) (deepSeekChatToolFields, error) {
	var fields deepSeekChatToolFields
	switch first {
	case json.Delim('{'), json.Delim('['):
		if depth > deepSeekChatJSONMaxDepth {
			return fields, errors.New("tool JSON exceeds maximum nesting depth")
		}
		if first == json.Delim('{') {
			return p.parseObject(depth, scope)
		}
		return fields, p.parseArray(depth, nil)
	}
	return fields, nil
}

func (p deepSeekChatToolParser) parseObject(depth int, scope deepSeekChatToolScope) (deepSeekChatToolFields, error) {
	var fields deepSeekChatToolFields
	seen := make(map[string]struct{})
	for {
		token, err := p.next()
		if err != nil || token == json.Delim('}') {
			return fields, err
		}
		key, ok := token.(string)
		if !ok {
			return fields, errDeepSeekInvalidJSON
		}
		if _, exists := seen[key]; exists {
			return fields, errors.New("duplicate JSON keys inside tools or tool_choice")
		}
		seen[key] = struct{}{}
		first, err := p.next()
		if err != nil {
			return fields, err
		}
		childScope := deepSeekChatToolNested
		if scope == deepSeekChatToolTop {
			switch key {
			case "type":
				fields.typ, _ = first.(string)
			case "name":
				fields.name, _ = first.(string)
			case "function":
				childScope = deepSeekChatToolFunction
			}
		} else if scope == deepSeekChatToolFunction && key == "name" {
			fields.name, _ = first.(string)
		}
		child, err := p.parseValue(first, depth+1, childScope)
		if err != nil {
			return fields, err
		}
		if childScope == deepSeekChatToolFunction {
			fields.functionName = child.name
		}
	}
}

func (p deepSeekChatToolParser) parseArray(depth int, element func(json.Token, int, int) error) error {
	for count := 0; p.dec.More(); count++ {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if count == deepSeekMaxRequestTools {
			return fmt.Errorf("tool JSON exceeds the tool count limit of %d elements per array", deepSeekMaxRequestTools)
		}
		offset := int(p.dec.InputOffset())
		first, err := p.next()
		if err != nil {
			return err
		}
		if element == nil {
			_, err = p.parseValue(first, depth+1, deepSeekChatToolNested)
		} else {
			err = element(first, depth+1, offset)
		}
		if err != nil {
			return err
		}
	}
	last, err := p.next()
	if err != nil {
		return err
	}
	if last != json.Delim(']') {
		return errDeepSeekInvalidJSON
	}
	return nil
}

// This family match is only for filtering a disabled group's Chat request.
// Hosted tool eligibility continues to use the existing provider rules.
func deepSeekChatWebToolFamily(typ string) string {
	for _, family := range [...]string{"web_search", "web_fetch", "deepseek_web_search", "deepseek_read_url"} {
		if strings.HasPrefix(typ, family) {
			return family
		}
	}
	return ""
}

type deepSeekChatChoiceTarget struct {
	typ, name string
	isObject  bool
}

func normalizeDeepSeekChatToolChoice(ctx context.Context, raw []byte) (deepSeekChatChoiceTarget, error) {
	p := newDeepSeekChatToolParser(ctx, raw)
	first, err := p.next()
	if err != nil {
		return deepSeekChatChoiceTarget{}, err
	}
	fields, err := p.parseValue(first, 2, deepSeekChatToolTop)
	if err != nil {
		return deepSeekChatChoiceTarget{}, err
	}
	if int(p.dec.InputOffset()) != len(raw) {
		return deepSeekChatChoiceTarget{}, errDeepSeekInvalidJSON
	}
	if first == json.Delim('{') {
		name := fields.name
		if name == "" {
			name = fields.functionName
		}
		return deepSeekChatChoiceTarget{typ: fields.typ, name: name, isObject: true}, nil
	}
	mode, _ := first.(string)
	return deepSeekChatChoiceTarget{typ: mode}, nil
}

func (c deepSeekChatChoiceTarget) needsDowngrade(keptMatch, removedMatch, toolsEmpty bool) bool {
	if !c.isObject {
		return toolsEmpty && c.typ == "required"
	}
	if c.name != "" {
		return !keptMatch && removedMatch
	}
	return removedMatch || c.typ == "tool" || c.typ == "function"
}

// filterDeepSeekChatWebTools only runs for a Chat request whose group has tools
// turned off. Copy raw slices for the surviving tools and unrelated fields.
func filterDeepSeekChatWebTools(ctx context.Context, body []byte, top deepSeekChatEnvelope) ([]byte, error) {
	if top.duplicate != "" {
		return nil, fmt.Errorf("duplicate top-level JSON key: %s", top.duplicate)
	}
	if top.tools == nil || body[top.tools.value.start] != '[' {
		return body, ctx.Err()
	}

	var choice deepSeekChatChoiceTarget
	if top.choice != nil {
		var err error
		choice, err = normalizeDeepSeekChatToolChoice(ctx, body[top.choice.value.start:top.choice.value.end])
		if err != nil {
			return nil, err
		}
	}

	raw := body[top.tools.value.start:top.tools.value.end]
	p := newDeepSeekChatToolParser(ctx, raw)
	if _, err := p.next(); err != nil { // Consume '[' before reading entries.
		return nil, err
	}
	var kept []deepSeekChatSpan
	var removed, removedMatch, keptMatch bool
	// Only this request's target needs membership checks in the two sets.
	removedLookup := choice.name
	if removedLookup == "" {
		removedLookup = choice.typ
	}
	err := p.parseArray(2, func(first json.Token, depth, offset int) error {
		tokenEnd := int(p.dec.InputOffset())
		tool, err := p.parseValue(first, depth, deepSeekChatToolTop)
		if err != nil {
			return err
		}
		if family := deepSeekChatWebToolFamily(tool.typ); family != "" {
			removed = true
			if choice.isObject && (removedLookup == tool.typ || removedLookup == family || (tool.name != "" && removedLookup == tool.name)) {
				removedMatch = true
			}
			return nil
		}
		if choice.name != "" && tool.matchesKeptName(choice.name) {
			keptMatch = true
		}
		start := tokenEnd - 1
		if _, container := first.(json.Delim); !container {
			// A scalar entry is outside the tool schema, but is kept verbatim.
			// Locate its first byte after the optional separator.
			cursor := deepSeekChatCursor{ctx: ctx, body: raw, pos: offset}
			cursor.skipSpace()
			if cursor.take(',') {
				cursor.skipSpace()
			}
			if cursor.err != nil {
				return cursor.err
			}
			start = cursor.pos
		}
		base := top.tools.value.start
		kept = append(kept, deepSeekChatSpan{base + start, base + int(p.dec.InputOffset())})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if int(p.dec.InputOffset()) != len(raw) {
		return nil, errDeepSeekInvalidJSON
	}
	if !removed {
		return body, ctx.Err()
	}

	var edits []deepSeekChatEdit
	removedFields := 0
	toolsEmpty := len(kept) == 0
	downgradeChoice := choice.needsDowngrade(keptMatch, removedMatch, toolsEmpty)
	if toolsEmpty {
		edits, removedFields = removeDeepSeekChatToolFields(top, downgradeChoice)
	} else {
		edits = append(edits, deepSeekChatEdit{span: top.tools.value, tools: kept})
		if downgradeChoice {
			edits = append(edits, deepSeekChatEdit{span: top.choice.value, replacement: `"auto"`})
		}
	}
	if choice.typ != "none" {
		edits = append(edits, deepSeekChatNoticeEdit(body, top, removedFields))
	}
	return applyDeepSeekChatEdits(ctx, body, edits)
}

type deepSeekChatEdit struct {
	span        deepSeekChatSpan
	replacement string
	tools       []deepSeekChatSpan
	// A prefix keeps the original span between prefix and replacement.
	prefix string
}

func removeDeepSeekChatToolFields(top deepSeekChatEnvelope, removeChoice bool) ([]deepSeekChatEdit, int) {
	first, second := top.tools, top.choice
	if !removeChoice {
		return []deepSeekChatEdit{removeDeepSeekChatFields(first, first)}, 1
	}
	if second.index < first.index {
		first, second = second, first
	}
	if second.index == first.index+1 {
		return []deepSeekChatEdit{removeDeepSeekChatFields(first, second)}, 2
	}
	return []deepSeekChatEdit{removeDeepSeekChatFields(first, first), removeDeepSeekChatFields(second, second)}, 2
}

func removeDeepSeekChatFields(first, last *deepSeekChatField) deepSeekChatEdit {
	start, end := first.comma, last.value.end
	if first.index == 0 {
		start = first.keyStart
		if last.nextComma >= 0 {
			end = last.nextComma + 1
		}
	}
	return deepSeekChatEdit{span: deepSeekChatSpan{start, end}}
}

var deepSeekChatNoticeMessage = `{"content":` + string(appendSJSONString(nil, deepSeekToolsUnavailableNotice)) + `,"role":"system"}`

func deepSeekChatNoticeEdit(body []byte, top deepSeekChatEnvelope, removedFields int) deepSeekChatEdit {
	field := top.messages
	if field == nil && top.input != nil {
		// A fixed Chat upstream can receive a raw Responses-shaped body.
		// Keep its input shape and add the notice to the same history.
		field = top.input
		if body[field.value.start] == '"' {
			return deepSeekChatEdit{
				span: field.value, prefix: `[{"content":`,
				replacement: `,"role":"user"},` + deepSeekChatNoticeMessage + "]",
			}
		}
	}
	if field == nil {
		prefix := ""
		if top.fields > removedFields {
			prefix = ","
		}
		return deepSeekChatEdit{span: deepSeekChatSpan{top.end, top.end}, replacement: prefix + `"messages":[` + deepSeekChatNoticeMessage + "]"}
	}
	if body[field.value.start] == '[' {
		replacement := deepSeekChatNoticeMessage
		if field.nonemptyArray {
			replacement = "," + replacement
		}
		at := field.value.end - 1
		return deepSeekChatEdit{span: deepSeekChatSpan{at, at}, replacement: replacement}
	}
	return deepSeekChatEdit{span: field.value, replacement: "[" + deepSeekChatNoticeMessage + "]"}
}

func applyDeepSeekChatEdits(ctx context.Context, body []byte, edits []deepSeekChatEdit) ([]byte, error) {
	// At most three top-level edits are reordered. Tool entries themselves
	// are already in wire order and are written directly into the output.
	slices.SortFunc(edits, func(a, b deepSeekChatEdit) int { return a.span.start - b.span.start })
	size := len(body)
	for _, edit := range edits {
		size += len(edit.replacement)
		if edit.prefix == "" {
			size -= edit.span.end - edit.span.start
		} else {
			size += len(edit.prefix)
		}
		if len(edit.tools) > 0 {
			size += len(edit.tools) + 1 // brackets and separating commas
			for _, tool := range edit.tools {
				size += tool.end - tool.start
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, size)
	last := 0
	for _, edit := range edits {
		var err error
		out, err = appendDeepSeekChatBytes(ctx, out, body[last:edit.span.start])
		if err != nil {
			return nil, err
		}
		if len(edit.tools) > 0 {
			out = append(out, '[')
			for i, tool := range edit.tools {
				if i > 0 {
					out = append(out, ',')
				}
				out, err = appendDeepSeekChatBytes(ctx, out, body[tool.start:tool.end])
				if err != nil {
					return nil, err
				}
			}
			out = append(out, ']')
		} else {
			if edit.prefix != "" {
				out = append(out, edit.prefix...)
				out, err = appendDeepSeekChatBytes(ctx, out, body[edit.span.start:edit.span.end])
				if err != nil {
					return nil, err
				}
			}
			out = append(out, edit.replacement...)
		}
		last = edit.span.end
	}
	return appendDeepSeekChatBytes(ctx, out, body[last:])
}

func appendDeepSeekChatBytes(ctx context.Context, out, raw []byte) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return out, nil
		}
		n := min(len(raw), deepSeekJSONChunk)
		out = append(out, raw[:n]...)
		raw = raw[n:]
	}
}
