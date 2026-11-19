package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// upstreamRejectionSSEObserver observes framing without rewriting the bytes
// delivered to clients. Most providers use one complete JSON data line; legal
// multiline data frames are joined before parsing, with a fixed memory bound.
type upstreamRejectionSSEObserver struct {
	observer   *UpstreamRejectionObserver
	enabled    bool
	eventType  string
	completion string
	partial    strings.Builder
	discard    bool
}

func newUpstreamRejectionSSEObserver(account *Account, status int) *upstreamRejectionSSEObserver {
	return &upstreamRejectionSSEObserver{
		observer: NewUpstreamRejectionObserver(account, status),
		enabled:  account != nil && account.GetUpstreamRejectionProfile() != "",
	}
}

func (o *upstreamRejectionSSEObserver) ObserveLine(line string) {
	if o == nil || !o.enabled {
		return
	}
	if line == "" {
		o.Finish()
		o.eventType, o.discard = "", false
		return
	}
	if event, ok := extractOpenAISSEEventLine(line); ok {
		o.eventType = event
		return
	}
	data, ok := extractOpenAISSEDataLine(line)
	if !ok || o.discard {
		return
	}
	if o.partial.Len() == 0 && (strings.TrimSpace(data) == "[DONE]" || json.Valid([]byte(data))) {
		o.observe(data)
		return
	}
	if o.partial.Len()+len(data)+1 > maxOpenAIConcatenatedJSONBytes {
		o.partial.Reset()
		o.discard = true
		return
	}
	if o.partial.Len() > 0 {
		_ = o.partial.WriteByte('\n')
	}
	_, _ = o.partial.WriteString(data)
	if json.Valid([]byte(o.partial.String())) {
		o.observe(o.partial.String())
		o.partial.Reset()
	}
}

func (o *upstreamRejectionSSEObserver) observe(data string) {
	if strings.TrimSpace(data) == "[DONE]" {
		// A framing sentinel confirms completion but must not erase a more
		// informative, already parsed Responses terminal event.
		if o.completion == "" {
			o.completion = "[DONE]"
		}
		return
	}
	// Type augmentation must not repair a truncated JSON document into evidence.
	if !json.Valid([]byte(data)) {
		return
	}
	switch event := effectiveOpenAISSEEventType([]byte(data), o.eventType); event {
	case "response.completed":
		o.completion = event
	case "response.done":
		if o.completion != "response.completed" {
			o.completion = event
		}
	}
	o.observer.ObserveSSEData([]byte(openAICompatPayloadWithEventType(data, o.eventType)))
}

func (o *upstreamRejectionSSEObserver) Finish() {
	if o == nil {
		return
	}
	if o.partial.Len() > 0 {
		o.observe(o.partial.String())
		o.partial.Reset()
	}
}

func (o *upstreamRejectionSSEObserver) Evidence() *UpstreamRejectionEvidence {
	if o == nil {
		return nil
	}
	return o.observer.Evidence()
}

func (o *upstreamRejectionSSEObserver) CompletionEvent() string {
	if o == nil || !o.enabled {
		return ""
	}
	return o.completion
}

// UpstreamRejectionError is deliberately not a failover error. A supplier has
// already accepted and charged this attempt; replaying it may charge it again.
type UpstreamRejectionError struct {
	Evidence *UpstreamRejectionEvidence
}

// UpstreamRejectionHTTPError preserves an actually received HTTP status for
// financial release decisions when the ordinary protocol error is not a
// failover error. It is never constructed for local validation or transport
// failures. Unwrap keeps the pre-existing retry/error semantics intact.
type UpstreamRejectionHTTPError struct {
	StatusCode int
	Cause      error
}

func (e *UpstreamRejectionHTTPError) Error() string {
	if e == nil || e.Cause == nil {
		return "upstream HTTP request failed"
	}
	return e.Cause.Error()
}

func (e *UpstreamRejectionHTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UpstreamRejectionError) Error() string {
	if e == nil || e.Evidence == nil {
		return "upstream content rejection"
	}
	return fmt.Sprintf("upstream content rejection (%s, HTTP %d)", e.Evidence.RuleID, e.Evidence.HTTPStatus)
}

func observeUpstreamRejectionBody(account *Account, status int, body []byte) *UpstreamRejectionEvidence {
	if account == nil || account.GetUpstreamRejectionProfile() == "" {
		return nil
	}
	observer := NewUpstreamRejectionObserver(account, status)
	if bodyHasSSEFraming(body) {
		forEachOpenAISSEFrame(string(body), func(eventType string, data []byte) {
			if !json.Valid(data) {
				return
			}
			observer.ObserveSSEData([]byte(openAICompatPayloadWithEventType(string(data), eventType)))
		})
	} else {
		observer.ObserveJSON(body)
	}
	return observer.Evidence()
}

func upstreamRejectionSSECompletion(account *Account, status int, body []byte) string {
	observer := newUpstreamRejectionSSEObserver(account, status)
	for _, line := range strings.Split(string(body), "\n") {
		observer.ObserveLine(strings.TrimSuffix(line, "\r"))
	}
	observer.Finish()
	return observer.CompletionEvent()
}

// Some vendor gateways reply to stream=true with a JSON refusal. Treating
// that body as an empty SSE stream would fail over and repeat the charged call.
func upstreamRejectionJSONResponse(account *Account, resp *http.Response) bool {
	return account != nil && account.GetUpstreamRejectionProfile() != "" && resp != nil &&
		strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json")
}

func upstreamRejectionCompletionEvent(account *Account, event string) string {
	if account == nil || account.GetUpstreamRejectionProfile() == "" {
		return ""
	}
	switch event {
	case "[DONE]", "response.completed", "response.done", "chat.completed":
		return event
	default:
		return ""
	}
}

func upstreamRejectionJSONCompletion(account *Account, body []byte) string {
	if account == nil || account.GetUpstreamRejectionProfile() == "" || !json.Valid(body) {
		return ""
	}
	root := gjson.ParseBytes(body)
	if root.Get("status").String() == "completed" {
		return "response.completed"
	}
	choices := root.Get("choices").Array()
	if len(choices) == 0 {
		return ""
	}
	for _, choice := range choices {
		if strings.TrimSpace(choice.Get("finish_reason").String()) == "" {
			return ""
		}
	}
	return "chat.completed"
}

func upstreamRejectionForwardResult(resp *http.Response, originalModel, billingModel, upstreamModel string, stream bool, startTime time.Time, evidence *UpstreamRejectionEvidence) *OpenAIForwardResult {
	result := &OpenAIForwardResult{
		Model: originalModel, BillingModel: billingModel, UpstreamModel: upstreamModel,
		Stream: stream, Duration: time.Since(startTime), UpstreamRejection: evidence,
	}
	if resp != nil {
		result.RequestID = firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
		result.UpstreamHeaders = resp.Header.Clone()
		result.ResponseHeaders = resp.Header.Clone()
	}
	return result
}

// writeUpstreamRejectionHTTP preserves the refusal's actual status without
// leaking arbitrary vendor error bodies or marking an otherwise healthy
// account unusable. Normal 2xx refusal bodies are forwarded unchanged elsewhere.
func writeUpstreamRejectionHTTP(c *gin.Context, evidence *UpstreamRejectionEvidence) error {
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(evidence.HTTPStatus, gin.H{"error": gin.H{
			"type":    "content_policy_violation",
			"code":    "upstream_content_rejected",
			"message": "The upstream provider rejected this request under its content policy.",
		}})
		MarkResponseCommitted(c)
	}
	return &UpstreamRejectionError{Evidence: evidence}
}
