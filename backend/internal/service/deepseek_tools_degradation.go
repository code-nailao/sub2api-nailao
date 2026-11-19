package service

import (
	"errors"
	"fmt"
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
	var removed []int
	for i, item := range items {
		if typ := item.Get("type").String(); match(typ) {
			removed = append(removed, i)
			removedNames[typ] = true
			if name := item.Get("name").String(); name != "" {
				removedNames[name] = true
			}
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
