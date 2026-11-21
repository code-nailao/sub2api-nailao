package websearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// ErrSearchOutcomeUnknown indicates a provider may have accepted the search.
// The manager must not repeat it with another provider.
var ErrSearchOutcomeUnknown = errors.New("websearch: search outcome unknown")

// searchJSON permits switching providers after an explicit HTTP 401 rejection
// or when the transport proves it could not have sent anything. Missing trace
// events, partial writes, and other failures after submission are ambiguous.
func searchJSON(client *http.Client, request *http.Request, provider string, result any) error {
	var obtainingConn, possiblySent atomic.Bool
	trace := &httptrace.ClientTrace{
		GetConn:              func(string) { obtainingConn.Store(true) },
		GotConn:              func(httptrace.GotConnInfo) { possiblySent.Store(true) },
		WroteHeaders:         func() { possiblySent.Store(true) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { possiblySent.Store(true) },
		GotFirstResponseByte: func() { possiblySent.Store(true) },
	}
	ctx := httptrace.WithClientTrace(request.Context(), trace)
	response, err := client.Do(request.WithContext(ctx))
	if err != nil {
		cause := fmt.Errorf("%s: request failed: %w", provider, err)
		if response != nil || possiblySent.Load() || !obtainingConn.Load() {
			return fmt.Errorf("%w: %w", ErrSearchOutcomeUnknown, cause)
		}
		return cause
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: status %d: authentication rejected", provider, response.StatusCode)
	case http.StatusRequestTimeout:
		return fmt.Errorf("%w: %s returned HTTP %d", ErrSearchOutcomeUnknown, provider, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("%w: %s: read body: %w", ErrSearchOutcomeUnknown, provider, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: status %d: %s", ErrSearchOutcomeUnknown, provider, response.StatusCode, truncateBody(body))
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("%w: %s: decode response: %w", ErrSearchOutcomeUnknown, provider, err)
	}
	return nil
}

const (
	maxResponseSize   = 1 << 20 // 1 MB
	errorBodyTruncLen = 200
)

// truncateBody returns a truncated string of body for error messages.
func truncateBody(body []byte) string {
	if len(body) <= errorBodyTruncLen {
		return string(body)
	}
	return string(body[:errorBodyTruncLen]) + "...(truncated)"
}
