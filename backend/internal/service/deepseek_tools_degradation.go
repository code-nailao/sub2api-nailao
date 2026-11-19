package service

import (
	"errors"
	"net/http"
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

// First-turn fallback has no tool-result slot, so explicitly disclose the
// unavailable web capability in the model input without changing user history.
func (r *deepSeekHostedRequest) addUnavailableNotice() {
	const notice = "The gateway's optional web tools are unavailable for this request. Answer using the conversation and existing knowledge when appropriate. If live information is needed, tell the user it could not be verified. Do not claim to have searched, fetched, or verified sources."
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
