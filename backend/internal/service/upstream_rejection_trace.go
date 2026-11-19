package service

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Response headers that identify one request on the supplier's side. The names
// are generic: the request ids an OpenAI-compatible gateway returns, and the
// ray id of the edge in front of it. The ray id is the only identifier left
// when the edge answers for the origin, which is what a 502 or 524 page is.
const (
	upstreamRejectionHeaderClientRequestID = "X-Client-Request-ID"
	upstreamRejectionHeaderRequestID       = "X-Request-ID"
	upstreamRejectionHeaderCFRay           = "CF-Ray"

	maxUpstreamRejectionTraceValueLen = 128
)

// upstreamRejectionTrace is what the direct upstream's response said about
// itself. It exists so a hold can be matched to one supplier bill line. It
// never decides whether, or how much, a customer is charged.
type upstreamRejectionTrace struct {
	Status            int
	UpstreamRequestID string
	XClientRequestID  string
	XRequestID        string
	CFRay             string
}

// validUpstreamRejectionTraceValue is the storage contract of one identifier:
// printable ASCII, at most 128 bytes. Empty means "not observed".
func validUpstreamRejectionTraceValue(v string) bool {
	if len(v) > maxUpstreamRejectionTraceValueLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// upstreamRejectionTraceValue turns a header value into one that satisfies the
// storage contract, or into "". A value that is too long or has other bytes is
// dropped, not repaired: a broken or hostile header must never make a
// settlement fail, and a cut or half-repaired identifier would not match the
// supplier's bill anyway.
func upstreamRejectionTraceValue(v string) string {
	v = strings.TrimSpace(v)
	if !validUpstreamRejectionTraceValue(v) {
		return ""
	}
	return v
}

// storableUpstreamRequestID keeps a request id the way the ledger has always
// stored it, unless storage would refuse it. It is cut to the width of
// usage_logs.upstream_request_id exactly as the usage row cuts it, and dropped
// when it holds a control byte or invalid UTF-8, which JSONB cannot store.
// Dropping loses an identifier; it never loses a settlement.
func storableUpstreamRequestID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > maxUsageUpstreamRequestIDLen {
		id = id[:maxUsageUpstreamRequestIDLen]
		for len(id) > 0 && !utf8.ValidString(id) {
			id = id[:len(id)-1]
		}
	}
	if !utf8.ValidString(id) {
		return ""
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return id
}

// upstreamRejectionTraceFromHeaders reads the fixed identifiers and, when the
// account names one, the header the account declares as its request id.
func upstreamRejectionTraceFromHeaders(account *Account, h http.Header) upstreamRejectionTrace {
	if len(h) == 0 {
		return upstreamRejectionTrace{}
	}
	return upstreamRejectionTrace{
		UpstreamRequestID: storableUpstreamRequestID(UpstreamRequestIDFromHeaders(account, h)),
		XClientRequestID:  upstreamRejectionTraceValue(h.Get(upstreamRejectionHeaderClientRequestID)),
		XRequestID:        upstreamRejectionTraceValue(h.Get(upstreamRejectionHeaderRequestID)),
		CFRay:             upstreamRejectionTraceValue(h.Get(upstreamRejectionHeaderCFRay)),
	}
}

// fill adds the observation to evidence and never replaces a value that is
// already there.
func (t upstreamRejectionTrace) fill(e *UpstreamRejectionBillingEvidence) {
	if e.OriginalStatus == 0 {
		e.OriginalStatus = t.Status
	}
	if e.UpstreamRequestID == "" {
		e.UpstreamRequestID = t.UpstreamRequestID
	}
	if e.XClientRequestID == "" {
		e.XClientRequestID = t.XClientRequestID
	}
	if e.XRequestID == "" {
		e.XRequestID = t.XRequestID
	}
	if e.CFRay == "" {
		e.CFRay = t.CFRay
	}
}

// WithUpstreamObservation returns e with the upstream status and identifiers of
// prev wherever e has none. The deadline release uses it, so the released row
// still shows what the upstream answered before the outcome became unknown.
func (e UpstreamRejectionBillingEvidence) WithUpstreamObservation(prev UpstreamRejectionBillingEvidence) UpstreamRejectionBillingEvidence {
	upstreamRejectionTrace{
		Status: prev.OriginalStatus, UpstreamRequestID: prev.UpstreamRequestID,
		XClientRequestID: prev.XClientRequestID, XRequestID: prev.XRequestID, CFRay: prev.CFRay,
	}.fill(&e)
	return e
}

// observeUpstreamRejectionResponse records the response of the request that
// carries a rejection-fee hold. It is called at the transport seam, right after
// the request that MarkUpstreamRejectionBillingSent persisted returns. An
// attempt sends once, so the first response is the only one; a request without
// a hold costs one context lookup.
func observeUpstreamRejectionResponse(ctx context.Context, resp *http.Response) {
	if resp == nil {
		return
	}
	attempt, _ := ctx.Value(upstreamRejectionAttemptContextKey{}).(*UpstreamRejectionBillingAttempt)
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.trace != nil {
		return
	}
	trace := upstreamRejectionTraceFromHeaders(attempt.Account, resp.Header)
	trace.Status = resp.StatusCode
	attempt.trace = &trace
}

// observedEvidence is the evidence of an outcome that has no rule behind it (a
// hold that stays unknown, or one released because the upstream refused the
// request without charging): the observed status and identifiers, nothing else.
// The result's headers fill what the transport seam did not see.
func (a *UpstreamRejectionBillingAttempt) observedEvidence(result *OpenAIForwardResult) UpstreamRejectionBillingEvidence {
	var e UpstreamRejectionBillingEvidence
	a.mu.Lock()
	if a.trace != nil {
		a.trace.fill(&e)
	}
	a.mu.Unlock()
	if result != nil && !result.OpenAIWSMode {
		upstreamRejectionTraceFromHeaders(a.Account, result.UpstreamHeaders).fill(&e)
	}
	return e
}
