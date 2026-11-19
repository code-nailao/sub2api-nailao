//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func grokMediaRejectionTestAccount() *Account {
	a := grokMediaContentTestAccount()
	a.Extra = map[string]any{
		"upstream_rejection_profile":  "grok_content_rejection_v1",
		"upstream_rejection_cost":     0.1,
		"upstream_rejection_currency": "USD",
	}
	// Charged 400/403 responses must remain terminal even when the supplier
	// account would otherwise allow ten same-account attempts for these codes.
	a.Credentials["pool_mode"] = true
	a.Credentials["pool_mode_retry_count"] = 10
	a.Credentials["pool_mode_retry_status_codes"] = []any{400, 403}
	return a
}

func grokMediaRejectionTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"supplier-request-1"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func TestForwardGrokMediaRejectionIsTerminalBeforeEmptyOutputOrPoolRetry(t *testing.T) {
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		model    string
		status   int
		body     string
	}{
		{
			name: "image generation moderation 400", endpoint: GrokMediaEndpointImagesGenerations,
			model: "grok-imagine-image", status: http.StatusBadRequest,
			body: `{"error":{"message":"Generated image rejected by content moderation"}}`,
		},
		{
			name: "image edit moderation 400", endpoint: GrokMediaEndpointImagesEdits,
			model: "grok-imagine-image-2.0", status: http.StatusBadRequest,
			body: `{"error":{"message":"generated image rejected by content moderation"}}`,
		},
		{
			name: "video generation moderation 400", endpoint: GrokMediaEndpointVideosGenerations,
			model: "grok-imagine-video", status: http.StatusBadRequest,
			body: `{"error":{"message":"generated video rejected by content moderation"}}`,
		},
		{
			name: "video generation policy 403", endpoint: GrokMediaEndpointVideosGenerations,
			model: "grok-imagine-video-1.5", status: http.StatusForbidden,
			body: `{"error":{"message":"request blocked by content moderation"}}`,
		},
		{
			name: "image 200 refusal without usage or images", endpoint: GrokMediaEndpointImagesGenerations,
			model: "grok-imagine-image", status: http.StatusOK,
			body: `{"choices":[{"message":{"role":"assistant","content":"I'm sorry, I cannot help with this request."}}]}`,
		},
		{
			name: "later choice refusal without images", endpoint: GrokMediaEndpointImagesGenerations,
			model: "grok-imagine-image-2.0", status: http.StatusOK,
			body: `{"choices":[{"index":0,"message":{"role":"assistant","content":"No image was produced."}},{"index":1,"message":{"role":"assistant","content":"Sorry, I can't help with that request."}}]}`,
		},
		{
			name: "responses output refusal without images", endpoint: GrokMediaEndpointImagesGenerations,
			model: "grok-imagine-image", status: http.StatusOK,
			body: `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I cannot help with your request."}]}]}`,
		},
		{
			name: "video create 200 refusal", endpoint: GrokMediaEndpointVideosGenerations,
			model: "grok-imagine-video", status: http.StatusOK,
			body: `{"choices":[{"message":{"role":"assistant","content":"I can’t help with the prompt."}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := grokMediaRejectionTestAccount()
			upstream := &grokMediaContentUpstreamStub{response: grokMediaRejectionTestResponse(tt.status, tt.body)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, recorder := grokMediaContentTestContext(http.MethodPost, "https://api.example/v1/images/generations", nil)
			body := []byte(`{"model":"` + tt.model + `","prompt":"A green square","image":{"url":"https://example.com/reference.png"}}`)
			result, err := svc.ForwardGrokMedia(context.Background(), c, account, tt.endpoint, "", body, "application/json")

			require.Error(t, err)
			require.NotNil(t, result, "a failed response must retain its billable refusal outcome")
			require.NotNil(t, result.UpstreamRejection)
			require.Equal(t, tt.status, result.UpstreamRejection.HTTPStatus)
			require.Equal(t, tt.status, recorder.Code)
			require.Equal(t, "supplier-request-1", result.RequestID)
			require.Zero(t, result.ImageCount)
			require.Zero(t, result.VideoCount)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "charged failures must never enter same-account or next-account retry")
			var rejection *UpstreamRejectionError
			require.ErrorAs(t, err, &rejection)
			require.Len(t, upstream.requests, 1)
			if tt.status == http.StatusOK {
				require.JSONEq(t, tt.body, recorder.Body.String(), "a 2xx refusal must preserve the original payload")
			}
		})
	}
}

func TestForwardGrokMediaRejectionDoesNotChargeOrdinaryErrorsOrOtherAccounts(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		profile bool
	}{
		{"ordinary image argument error", http.StatusBadRequest, `{"error":{"message":"invalid image resolution"}}`, true},
		{"auth error is not moderation", http.StatusForbidden, `{"error":{"message":"Invalid API key"}}`, true},
		{"moderation text on rate limit", http.StatusTooManyRequests, `{"error":{"message":"content moderation"}}`, true},
		{"plain refusal without opted-in profile", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"Sorry, I cannot help with this request."}}]}`, false},
		{"moderation 400 without opted-in profile", http.StatusBadRequest, `{"error":{"message":"generated image rejected by content moderation"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := grokMediaRejectionTestAccount()
			if !tt.profile {
				delete(account.Extra, "upstream_rejection_profile")
			}
			upstream := &grokMediaContentUpstreamStub{response: grokMediaRejectionTestResponse(tt.status, tt.body)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, _ := grokMediaContentTestContext(http.MethodPost, "https://api.example/v1/images/generations", nil)
			result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations, "", []byte(`{"model":"grok-imagine-image","prompt":"A green square"}`), "application/json")
			require.Error(t, err)
			if result != nil {
				require.Nil(t, result.UpstreamRejection)
			}
			var rejection *UpstreamRejectionError
			require.False(t, errors.As(err, &rejection))
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestForwardGrokMediaContentRejectionDoesNotDownloadContent(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := `{"error":{"message":"generated video rejected by content moderation"}}`
			if status == http.StatusOK {
				body = `{"status":"failed","choices":[{"message":{"role":"assistant","content":"I cannot help with that request."}}]}`
			}
			upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
				grokMediaRejectionTestResponse(status, body),
				{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("must-not-be-downloaded"))},
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, recorder := grokMediaContentTestContext(http.MethodGet, "https://api.example/v1/videos/task-1/content", nil)
			result, err := svc.ForwardGrokMedia(context.Background(), c, grokMediaRejectionTestAccount(), GrokMediaEndpointVideoContent, "task-1", nil, "")
			require.Error(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.UpstreamRejection)
			require.Equal(t, status, recorder.Code)
			require.Equal(t, status, result.UpstreamRejection.HTTPStatus)
			require.Zero(t, result.ImageCount)
			require.Zero(t, result.VideoCount)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "/v1/videos/task-1", upstream.requests[0].URL.Path)
			require.NotContains(t, recorder.Body.String(), "must-not-be-downloaded")
		})
	}
}

func TestForwardGrokMediaVideoFailedHTTP200DoesNotInventChargeableStatus(t *testing.T) {
	upstreamBody := `{"request_id":"task-1","status":"failed","error":{"message":"generated video rejected by content moderation"}}`
	upstream := &grokMediaContentUpstreamStub{response: grokMediaRejectionTestResponse(http.StatusOK, upstreamBody)}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext(http.MethodGet, "https://api.example/v1/videos/task-1", nil)
	result, err := svc.ForwardGrokMedia(context.Background(), c, grokMediaRejectionTestAccount(), GrokMediaEndpointVideoStatus, "task-1", nil, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.UpstreamRejection, "a 200 failed task does not satisfy the supplied 400/403 contract")
	require.Zero(t, result.ImageCount)
	require.Zero(t, result.VideoCount)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, upstreamBody, recorder.Body.String())
	require.Len(t, upstream.requests, 1)
}

func TestForwardGrokMediaSignedDownloadPolicyErrorIsNotGenerationRejection(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
		grokMediaContentStatusResponse(`{"request_id":"task-1","status":"done","video":{"url":"https://vidgen.x.ai/signed/xai-video-task-1.mp4","duration":1}}`),
		grokMediaRejectionTestResponse(http.StatusForbidden, `{"error":{"message":"content policy violation"}}`),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext(http.MethodGet, "https://api.example/v1/videos/task-1/content", nil)
	result, err := svc.ForwardGrokMedia(context.Background(), c, grokMediaRejectionTestAccount(), GrokMediaEndpointVideoContent, "task-1", nil, "")
	require.Error(t, err)
	require.Nil(t, result)
	var rejected *UpstreamRejectionError
	require.False(t, errors.As(err, &rejected), "a CDN download does not establish supplier generation moderation billing")
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Len(t, upstream.requests, 2)
	require.Empty(t, upstream.requests[1].Header.Get("Authorization"))
}

// Cross the actual admission -> HTTP forwarding -> financial finish seam. A
// real HTTP parameter/auth/rate error is not an unknown transport outcome and
// must release the admitted 0.1 reservation without another supplier request.
func TestGrokMediaOrdinaryHTTPErrorReleasesAdmittedFixedFee(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			svc, ledger, key, account := rejectionFacadeFixture()
			account.ID = 900000 + int64(status) // isolate ordinary auth/rate cooldown state from other fixtures
			svc.cfg = &config.Config{}
			account.Credentials["pool_mode"] = false
			upstream := &grokMediaContentUpstreamStub{response: grokMediaRejectionTestResponse(status, `{"error":{"message":"invalid image resolution"}}`)}
			svc.httpUpstream = upstream
			body := []byte(`{"model":"grok-imagine-image","prompt":"A green square"}`)
			attempt, err := svc.BeginUpstreamRejectionBilling(context.Background(), key, account, "grok-imagine-image", body)
			require.NoError(t, err)
			require.Equal(t, .1, attempt.Reservation.Price)
			ctx := WithUpstreamRejectionBillingAttempt(context.Background(), attempt)
			c, _ := grokMediaContentTestContext(http.MethodPost, "https://local.example/v1/images/generations", nil)
			result, forwardErr := svc.ForwardGrokMedia(ctx, c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
			require.Nil(t, result)
			require.Error(t, forwardErr)
			var observed *UpstreamRejectionHTTPError
			require.ErrorAs(t, forwardErr, &observed)
			require.Equal(t, status, observed.StatusCode)
			finishErr := svc.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, forwardErr)
			if status == http.StatusInternalServerError {
				require.ErrorIs(t, finishErr, ErrUpstreamRejectionOutcomeUnknown)
				require.Equal(t, UpstreamRejectionUnknown, ledger.row.State)
			} else {
				require.ErrorIs(t, finishErr, forwardErr)
				require.Equal(t, UpstreamRejectionReleased, ledger.row.State)
			}
			require.Zero(t, ledger.captures)
			require.Equal(t, 1, ledger.finalizeCalls)
			require.Equal(t, 1, ledger.markCalls)
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestGrokMediaDownstreamErrorRewritePreservesOriginalBillingStatus(t *testing.T) {
	svc, ledger, key, account := rejectionFacadeFixture()
	svc.cfg = &config.Config{}
	account.Credentials["custom_error_codes_enabled"] = true
	account.Credentials["custom_error_codes"] = []any{float64(429)}
	upstream := &grokMediaContentUpstreamStub{response: grokMediaRejectionTestResponse(400, `{"error":{"message":"invalid image resolution"}}`)}
	svc.httpUpstream = upstream
	body := []byte(`{"model":"grok-imagine-image","prompt":"A green square"}`)
	attempt, err := svc.BeginUpstreamRejectionBilling(context.Background(), key, account, "grok-imagine-image", body)
	require.NoError(t, err)
	ctx := WithUpstreamRejectionBillingAttempt(context.Background(), attempt)
	c, recorder := grokMediaContentTestContext(http.MethodPost, "https://local.example/v1/images/generations", nil)
	result, forwardErr := svc.ForwardGrokMedia(ctx, c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
	require.Equal(t, 500, recorder.Code, "existing downstream custom-error transformation is preserved")
	var observed *UpstreamRejectionHTTPError
	require.ErrorAs(t, forwardErr, &observed)
	require.Equal(t, 400, observed.StatusCode, "the rewritten response cannot change financial classification")
	require.Error(t, svc.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, forwardErr))
	require.Equal(t, UpstreamRejectionReleased, ledger.row.State)
	require.Zero(t, ledger.captures)
	require.Len(t, upstream.requests, 1)
}
