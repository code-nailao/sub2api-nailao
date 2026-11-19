//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Upstream's pre-output idle failover penalises the account with
// HandleStreamTimeout. It only runs for OpenAI accounts, and content rejection
// evidence is only collected for Grok API-key accounts, so a stalled stream
// that already carried a refusal can never reach that penalty. If rejection
// profiles are ever extended to OpenAI, this test fails and the failover must
// first be guarded against existing rejection evidence.
func TestOpenAIIdleFailoverCannotHoldRejectionEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: accountType, Extra: map[string]any{
				"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1,
			}}
			require.Empty(t, account.GetUpstreamRejectionProfile())

			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 1,
			}}}
			pr, pw := io.Pipe()
			defer func() { _ = pr.Close() }()
			defer func() { _ = pw.Close() }()
			go func() {
				_, _ = io.WriteString(pw, strings.Join([]string{
					`data: {"type":"response.created","response":{"id":"r"}}`, "",
					`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m","type":"message","role":"assistant","status":"in_progress","content":[]}}`, "", "",
				}, "\n"))
			}()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}
			result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "the idle failover stays available for OpenAI")
			var rejection *UpstreamRejectionError
			require.False(t, errors.As(err, &rejection))
			if result != nil {
				require.Nil(t, result.upstreamRejection)
			}
		})
	}
}
