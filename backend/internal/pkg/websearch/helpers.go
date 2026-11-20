package websearch

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrSearchOutcomeUnknown indicates a provider may have accepted the search.
// The manager must not repeat it with another provider.
var ErrSearchOutcomeUnknown = errors.New("websearch: search outcome unknown")

func searchSubmissionTimeout(provider string) error {
	return fmt.Errorf("%w: %s returned HTTP %d", ErrSearchOutcomeUnknown, provider, http.StatusRequestTimeout)
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
