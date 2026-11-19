package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rejectionHandlerUpstream struct {
	service.HTTPUpstream
	calls atomic.Int32
}

type rejectionPlatformQuotaRepository struct {
	service.UsageBillingRepository
	service.UpstreamRejectionBillingRepository
	reserves atomic.Int32
}

func (r *rejectionPlatformQuotaRepository) ReserveUpstreamRejection(context.Context, *service.UpstreamRejectionReserveCommand) (*service.UpstreamRejectionBillingResult, error) {
	r.reserves.Add(1)
	return nil, service.ErrUpstreamRejectionPlatformQuotaUnsupported
}

func (u *rejectionHandlerUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"request blocked by content moderation"}}`)),
	}, nil
}

func TestUpstreamRejectionHTTPHandlersDoNotRetrySupplierCharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			h, upstream, key := newRejectionHTTPTestHandler(t, false)
			rec := serveRejectionHTTPTest(t, h, key, endpoint)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, upstream.calls.Load(), "a paid refusal must not enter pool retries or account failover even when customer fee is disabled")
		})
	}
}

func TestUpstreamRejectionHTTPHandlersFailClosedWithoutLedger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			h, upstream, key := newRejectionHTTPTestHandler(t, true)
			rec := serveRejectionHTTPTest(t, h, key, endpoint)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Zero(t, upstream.calls.Load(), "no supplier operation may be sent without durable money admission")
		})
	}
}

func TestUpstreamRejectionHTTPHandlersPlatformQuotaRejectedBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			ledger := &rejectionPlatformQuotaRepository{}
			h, upstream, key := newRejectionHTTPTestHandler(t, true, ledger)
			rec := serveRejectionHTTPTest(t, h, key, endpoint)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "upstream_rejection_platform_quota_unsupported")
			require.Contains(t, rec.Body.String(), "当前用户的平台限额与此收费策略暂不兼容，请联系管理员")
			require.EqualValues(t, 1, ledger.reserves.Load())
			require.Zero(t, upstream.calls.Load(), "unsupported platform quota must reject before sending a supplier request")
		})
	}
}

// rejectionRecordingLedger reserves like the real ledger and keeps how the
// hold ended. It is used from the request goroutine only.
type rejectionRecordingLedger struct {
	service.UsageBillingRepository
	service.UpstreamRejectionBillingRepository
	row       *service.UpstreamRejectionReservation
	sent      int
	finalized []service.UpstreamRejectionFinalizeCommand
}

func (l *rejectionRecordingLedger) ReserveUpstreamRejection(_ context.Context, c *service.UpstreamRejectionReserveCommand) (*service.UpstreamRejectionBillingResult, error) {
	l.row = &service.UpstreamRejectionReservation{UpstreamRejectionReserveCommand: *c, State: service.UpstreamRejectionReserved}
	return &service.UpstreamRejectionBillingResult{Applied: true, Reservation: l.row}, nil
}

func (l *rejectionRecordingLedger) MarkUpstreamRejectionSent(context.Context, string, int64) (*service.UpstreamRejectionBillingResult, error) {
	l.sent++
	l.row.State = service.UpstreamRejectionSent
	return &service.UpstreamRejectionBillingResult{Applied: true, Reservation: l.row}, nil
}

func (l *rejectionRecordingLedger) FinalizeUpstreamRejection(_ context.Context, c *service.UpstreamRejectionFinalizeCommand) (*service.UpstreamRejectionBillingResult, error) {
	l.finalized = append(l.finalized, *c)
	l.row.State = c.Outcome
	return &service.UpstreamRejectionBillingResult{Applied: true, Reservation: l.row}, nil
}

const rejectionComposerImageRequest = `{"model":"grok-composer-2.5-fast","messages":[{"role":"user","content":[{"type":"text","text":"What is shown?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}],"stream":false}`

// A hold covers one upstream request and describing images would send more, so
// a composer request with image input is refused with a clear 400: nothing is
// sent to the supplier and the hold is released at once without charge.
func TestUpstreamRejectionChatRefusesComposerImageInputAndReleasesTheHold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ledger := &rejectionRecordingLedger{}
	h, upstream, key := newRejectionHTTPTestHandler(t, true, ledger)
	rec := serveRejectionHTTPBody(t, h, key, "chat/completions", rejectionComposerImageRequest)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"error":{"type":"invalid_request_error","message":"This model does not support image input in this group"}}`, rec.Body.String())
	require.Zero(t, upstream.calls.Load(), "nothing may reach the supplier")
	require.NotNil(t, ledger.row, "the fixed fee was reserved for this request")
	require.Zero(t, ledger.sent, "the hold was never marked as sent")
	require.Equal(t, []service.UpstreamRejectionFinalizeCommand{{
		RequestID: ledger.row.RequestID, APIKeyID: key.ID,
		Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true,
	}}, ledger.finalized, "released once, as a request that was never sent")
}

// A group that does not charge the fee carries no hold: the same request still
// sends the image-description call first. The fixture's upstream answers every
// call with a 403, so the description fails and the composer request does not
// follow.
func TestUpstreamRejectionChatWithoutTheFeeStillDescribesComposerImages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, upstream, key := newRejectionHTTPTestHandler(t, false)
	rec := serveRejectionHTTPBody(t, h, key, "chat/completions", rejectionComposerImageRequest)

	require.EqualValues(t, 1, upstream.calls.Load(), "the description call is sent")
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "grok composer image bridge upstream error")
}

func TestUpstreamRejectionWebSocketRejectedBeforeUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, upstream, key := newRejectionHTTPTestHandler(t, true)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Sec-WebSocket-Version", "13")
	c.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	c.Set(string(middleware2.ContextKeyAPIKey), key)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID, Concurrency: 1})
	h.ResponsesWebSocket(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unsupported_transport")
	require.Zero(t, upstream.calls.Load())
}

func TestUpstreamRejectionWebSocketGateDoesNotAffectExistingGroups(t *testing.T) {
	require.False(t, requiresUpstreamRejectionHTTP(nil))
	require.False(t, requiresUpstreamRejectionHTTP(&service.APIKey{}))
	require.False(t, requiresUpstreamRejectionHTTP(&service.APIKey{Group: &service.Group{Platform: service.PlatformGrok}}))
	require.True(t, requiresUpstreamRejectionHTTP(&service.APIKey{Group: &service.Group{Platform: service.PlatformGrok, UpstreamRejectionBillingEnabled: true}}))
}

func newRejectionHTTPTestHandler(t *testing.T, enabled bool, ledgers ...service.UsageBillingRepository) (*OpenAIGatewayHandler, *rejectionHandlerUpstream, *service.APIKey) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxAccountSwitches = 3
	groupID := int64(99123)
	account := service.Account{
		ID: 99123, Name: "rejection-handler-fixture", Platform: service.PlatformGrok,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key": "test-not-a-real-key", "base_url": "https://provider.example.test",
			"model_mapping": map[string]any{"grok-4.5": "grok-4.5", "grok-composer-2.5-fast": "grok-composer-2.5-fast"},
			"pool_mode":     true, "pool_mode_retry_count": float64(10),
			"pool_mode_retry_status_codes": []any{float64(400), float64(403)},
		},
		Extra: map[string]any{"upstream_rejection_profile": "grok_content_rejection_v1", "upstream_rejection_cost": 0.1, "upstream_rejection_currency": "USD"},
	}
	repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	upstream := &rejectionHandlerUpstream{}
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gatewayConfig := *cfg
	if enabled {
		// Isolate the durable gateway admission from the existing cache preflight.
		// The gateway itself must use standard mode to reach the missing-ledger check.
		gatewayConfig.RunMode = config.RunModeStandard
	}
	var ledger service.UsageBillingRepository
	if len(ledgers) > 0 {
		ledger = ledgers[0]
	}
	gateway := service.NewOpenAIGatewayService(repo, usage, ledger, nil, nil, nil, nil, &gatewayConfig, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil,
		service.NewGrokTokenProvider(repo, nil), nil, nil, nil, nil, nil)
	concurrency := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(concurrency), billingCache,
		&service.APIKeyService{}, nil, nil, nil, nil, cfg)
	key := &service.APIKey{
		ID: 99124, UserID: 99125, GroupID: &groupID,
		User: &service.User{ID: 99125, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, Status: service.StatusActive,
			SubscriptionType: "standard", RateMultiplier: 0.13,
			UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.1},
	}
	if enabled && ledger == nil {
		_, err := gateway.BeginUpstreamRejectionBilling(context.Background(), key, &account, "grok-4.5", []byte(`{"model":"grok-4.5"}`))
		require.ErrorIs(t, err, service.ErrUpstreamRejectionBillingUnavailable)
	}
	return h, upstream, key
}

func serveRejectionHTTPTest(t *testing.T, h *OpenAIGatewayHandler, key *service.APIKey, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"grok-4.5","input":"hello","stream":false}`
	if endpoint != "responses" {
		body = `{"model":"grok-4.5","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":false}`
	}
	return serveRejectionHTTPBody(t, h, key, endpoint, body)
}

func serveRejectionHTTPBody(t *testing.T, h *OpenAIGatewayHandler, key *service.APIKey, endpoint, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), key)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID, Concurrency: 1})
	switch endpoint {
	case "responses":
		h.Responses(c)
	case "chat/completions":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	default:
		t.Fatal("unknown test endpoint")
	}
	return rec
}
