package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This result is gateway-owned evidence of unavailability, never a fabricated
// search result. It remains in the conversation on the ordinary continuation.
const deepSeekToolsUnavailableResult = `{"error":"Web tools are unavailable for this request. Continue using the conversation and existing knowledge when appropriate. Tell the user that live information could not be verified. Do not claim to have searched, fetched, or verified sources; do not retry web tools."}`

func (r *deepSeekHostedRequest) disableServerTools() {
	r.ToolsUnavailable = true
	for _, op := range r.Owned {
		r.Limits[op] = 0
	}
	r.removeExhaustedTools()
	if tools, _ := r.Body["tools"].([]any); len(tools) == 0 {
		// Omit tool_choice entirely for a plain model request. Some thinking
		// providers reject "none" even when no tool definitions remain.
		delete(r.Body, "tools")
		delete(r.Body, "tool_choice")
		return
	}
	// Preserve client-owned forced choices; only a forced hosted tool became invalid.
	choice, _ := r.Body["tool_choice"].(map[string]any)
	name, _ := choice["name"].(string)
	if r.Protocol == "chat" {
		function, _ := choice["function"].(map[string]any)
		name, _ = function["name"].(string)
	}
	// A body whose tools value is not a list has no tool to fall back to; treat
	// it as empty instead of panicking on the assertion.
	tools, _ := r.Body["tools"].([]any)
	if _, owned := r.Owned[name]; owned && len(tools) > 0 {
		if r.Protocol == "messages" {
			r.Body["tool_choice"] = map[string]any{"type": "auto"}
		} else {
			r.Body["tool_choice"] = "auto"
		}
	}
}

func deepSeekCanFallbackWithoutTools(err error, httpStatus int) bool {
	var upstream *UpstreamFailoverError
	if !errors.As(err, &upstream) || httpStatus != upstream.StatusCode || upstream.IsCredentialFailure() || upstream.NextAccountAction == NextAccountStop {
		return false
	}
	// No network errors, unknown outcomes, rate limits, authorization denials,
	// incomplete 200 streams or context deadlines are replayed by this fallback.
	switch upstream.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// deepSeekToolsUnavailableNotice tells the model that the web tools it was
// offered cannot be used, so it answers without claiming to have searched.
const deepSeekToolsUnavailableNotice = "The gateway's optional web tools are unavailable for this request. Answer using the conversation and existing knowledge when appropriate. If live information is needed, tell the user it could not be verified. Do not claim to have searched, fetched, or verified sources."

const (
	deepSeekChatJSONMaxDepth  = 128
	deepSeekChatJSONReadChunk = 4 << 10
)

// Bound each decoder refill so cancellation is observed even inside one large
// string token. The request bytes were already bounded by ingress middleware.
type deepSeekChatJSONReader struct {
	ctx  context.Context
	body []byte
}

func (r *deepSeekChatJSONReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	if len(p) > deepSeekChatJSONReadChunk {
		p = p[:deepSeekChatJSONReadChunk]
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, nil
}

// Read tokens once in wire order instead of rescanning each ancestor's raw
// subtree. Decoded object keys catch escaped duplicates without re-encoding the
// request, and UseNumber avoids float conversion of otherwise untouched values.
func validateDeepSeekChatJSON(ctx context.Context, body []byte) error {
	decoder := json.NewDecoder(&deepSeekChatJSONReader{ctx: ctx, body: body})
	decoder.UseNumber()
	invalid := errors.New("invalid JSON in Chat request")
	next := func() (json.Token, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != io.EOF {
				return nil, invalid
			}
		}
		return token, err
	}
	first, err := next()
	if err != nil && err != io.EOF {
		return err
	}
	if first != json.Delim('{') {
		return invalid
	}
	type scope struct {
		keys    map[string]struct{}
		wantKey bool
	}
	stack := []scope{{keys: make(map[string]struct{}), wantKey: true}}
	for len(stack) > 0 {
		token, err := next()
		if err != nil {
			if err == io.EOF {
				return invalid
			}
			return err
		}
		delim, container := token.(json.Delim)
		if container && (delim == '}' || delim == ']') {
			stack[len(stack)-1] = scope{}
			stack = stack[:len(stack)-1]
			continue
		}
		current := &stack[len(stack)-1]
		if current.keys != nil {
			if current.wantKey {
				key, ok := token.(string)
				if !ok {
					return invalid
				}
				if _, duplicate := current.keys[key]; duplicate {
					return errors.New("duplicate JSON fields are not supported in Chat requests without web tools")
				}
				current.keys[key] = struct{}{}
				current.wantKey = false
				continue
			}
			current.wantKey = true
		}
		if container {
			if len(stack) >= deepSeekChatJSONMaxDepth {
				return errors.New("chat request JSON exceeds maximum nesting depth")
			}
			child := scope{}
			if delim == '{' {
				child.keys = make(map[string]struct{})
				child.wantKey = true
			}
			stack = append(stack, child)
		}
	}
	if _, err := next(); err != io.EOF {
		if err != nil {
			return err
		}
		return invalid
	}
	return nil
}

// First-turn fallback has no tool-result slot, so explicitly disclose the
// unavailable web capability in the model input without changing user history.
func (r *deepSeekHostedRequest) addUnavailableNotice() {
	const notice = deepSeekToolsUnavailableNotice
	switch r.Protocol {
	case "messages":
		switch system := r.Body["system"].(type) {
		case string:
			r.Body["system"] = system + "\n\n" + notice
		case []any:
			r.Body["system"] = append(system, map[string]any{"type": "text", "text": notice})
		default:
			r.Body["system"] = notice
		}
	case "responses":
		var input []any
		switch v := r.Body["input"].(type) {
		case string:
			input = []any{map[string]any{"role": "user", "content": v}}
		case []any:
			input = v
		}
		r.Body["input"] = append(input, map[string]any{"role": "system", "content": notice})
	default:
		messages, _ := r.Body["messages"].([]any)
		r.Body["messages"] = append(messages, map[string]any{"role": "system", "content": notice})
	}
}

// withoutDeepSeekWebTools removes the tools entries whose type match selects,
// editing the request in place so every other byte stays as the client sent it.
// A tool_choice that forced a removed tool no longer forces one; when no tool
// is left, tools and tool_choice are both deleted, as disableServerTools does.
// With notice set, the unavailable-web notice is added where
// addUnavailableNotice puts it. It reports whether anything was removed.
func withoutDeepSeekWebTools(body []byte, protocol string, match func(typ string) bool, notice bool) ([]byte, bool, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, false, nil
	}
	items := tools.Array()
	removedNames := map[string]bool{}
	clientNames := map[string]bool{}
	var removed []int
	for i, item := range items {
		if typ := item.Get("type").String(); match(typ) {
			removed = append(removed, i)
			removedNames[typ] = true
			if name := item.Get("name").String(); name != "" {
				removedNames[name] = true
			}
		} else if protocol == "chat" && item.Get("type").String() == "function" {
			clientNames[item.Get("function.name").String()] = true
		}
	}
	if len(removed) == 0 {
		return body, false, nil
	}
	out, err := body, error(nil)
	if len(removed) == len(items) {
		if out, err = sjson.DeleteBytes(out, "tools"); err == nil && gjson.GetBytes(out, "tool_choice").Exists() {
			out, err = sjson.DeleteBytes(out, "tool_choice")
		}
	} else {
		for i := len(removed) - 1; i >= 0 && err == nil; i-- {
			out, err = sjson.DeleteBytes(out, fmt.Sprintf("tools.%d", removed[i]))
		}
		choice := gjson.GetBytes(out, "tool_choice")
		typ, name := choice.Get("type").String(), choice.Get("name").String()
		forced := choice.IsObject() && (removedNames[typ] || match(typ) || (typ == "tool" && (removedNames[name] || match(name))))
		if protocol == "chat" && typ == "function" {
			name = choice.Get("function.name").String()
			// A client function may legitimately share a removed tool's name.
			forced = (removedNames[name] || match(name)) && !clientNames[name]
		}
		if err == nil && forced {
			auto := `"auto"`
			if protocol == "messages" {
				auto = `{"type":"auto"}`
			}
			out, err = sjson.SetRawBytes(out, "tool_choice", []byte(auto))
		}
	}
	if err == nil && notice {
		out, err = withDeepSeekUnavailableNotice(out, protocol)
	}
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// withDeepSeekUnavailableNotice adds deepSeekToolsUnavailableNotice in the
// place addUnavailableNotice uses, without re-encoding the rest of the body.
func withDeepSeekUnavailableNotice(body []byte, protocol string) ([]byte, error) {
	message := map[string]any{"role": "system", "content": deepSeekToolsUnavailableNotice}
	switch protocol {
	case "messages":
		system := gjson.GetBytes(body, "system")
		switch {
		case system.Type == gjson.String:
			return sjson.SetBytes(body, "system", system.String()+"\n\n"+deepSeekToolsUnavailableNotice)
		case system.IsArray():
			return sjson.SetBytes(body, "system.-1", map[string]any{"type": "text", "text": deepSeekToolsUnavailableNotice})
		default:
			return sjson.SetBytes(body, "system", deepSeekToolsUnavailableNotice)
		}
	case "responses":
		input := gjson.GetBytes(body, "input")
		switch {
		case input.IsArray():
			return sjson.SetBytes(body, "input.-1", message)
		case input.Type == gjson.String:
			return sjson.SetBytes(body, "input", []any{map[string]any{"role": "user", "content": input.String()}, message})
		default:
			return sjson.SetBytes(body, "input", []any{message})
		}
	default:
		if gjson.GetBytes(body, "messages").IsArray() {
			return sjson.SetBytes(body, "messages.-1", message)
		}
		return sjson.SetBytes(body, "messages", []any{message})
	}
}
