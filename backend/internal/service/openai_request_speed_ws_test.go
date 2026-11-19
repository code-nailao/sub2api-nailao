package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func fixedSpeedWSAccount(tier string) *Account {
	return &Account{
		ID: 7101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		GroupIDs: []int64{71}, RequestSpeedTiersByGroup: map[string][]string{"71": {tier}},
		Credentials: map[string]any{"api_key": "sk-fixed-test"},
	}
}

func TestRequestSpeedWSPreviousResponseBindingCannotCrossTier(t *testing.T) {
	ctx := WithRequestSpeed(context.Background(), 71, RequestSpeedFast)
	account := fixedSpeedWSAccount(RequestSpeedUltrafast)
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{*account}}, cache: cache, openaiWSStateStore: store}
	require.NoError(t, store.BindResponseAccount(ctx, 71, "resp_fixed_ultra", account.ID, time.Hour))
	groupID := int64(71)
	for _, gid := range []*int64{nil, &groupID} {
		require.ErrorIs(t, svc.ValidatePreviousResponseRequestSpeed(ctx, gid, "resp_fixed_ultra"), ErrRequestSpeedContinuationMismatch)
	}
	selected, err := svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_fixed_ultra", "gpt-6.1-sol", nil, false)
	require.ErrorIs(t, err, ErrRequestSpeedContinuationMismatch)
	require.Nil(t, selected)
	bound, err := store.GetResponseAccount(ctx, 71, "resp_fixed_ultra")
	require.NoError(t, err)
	require.Equal(t, account.ID, bound, "a rejected continuation must preserve its owner")
	require.NoError(t, svc.ValidatePreviousResponseRequestSpeed(WithRequestSpeed(context.Background(), 71, RequestSpeedUltrafast), &groupID, "resp_fixed_ultra"))
	require.NoError(t, svc.ValidatePreviousResponseRequestSpeed(context.Background(), &groupID, "resp_fixed_ultra"), "ordinary groups retain existing behavior")
}

func TestRequestSpeedWSHTTPBridgeKeepsSupplierTierAndStripsClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		t.Run(tier, func(t *testing.T) {
			ctx := WithRequestSpeed(context.Background(), 71, tier)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixed\",\"model\":\"gpt-6.1-sol\",\"status\":\"completed\",\"service_tier\":\"default\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20}}}\n\n"))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream}
			account := fixedSpeedWSAccount(tier)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
			payload := []byte(`{"type":"response.create","model":"gpt-6.1-sol","stream":true,"service_tier":"flex","input":"hi"}`)
			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "sk-fixed-test", payload, len(payload), "gpt-6.1-sol", "", "", "", "", 1, func([]byte) error { return nil })
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists())
			require.Equal(t, RequestSpeedServiceTierFromContext(ctx), result.FixedUpstreamRequestTier)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
		})
	}
}

type requestSpeedWSAccountRepo struct {
	AccountRepository
	mu      sync.RWMutex
	account Account
}

func (r *requestSpeedWSAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	copy := r.account
	return &copy, nil
}
func (r *requestSpeedWSAccountRepo) revokeMapping() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account.RequestSpeedTiersByGroup = map[string][]string{}
	r.account.AccountGroups = nil
}

func TestRequestSpeedWSLatestMappingRechecked(t *testing.T) {
	ctx := WithRequestSpeed(context.Background(), 71, RequestSpeedFast)
	account := fixedSpeedWSAccount(RequestSpeedFast)
	repo := &requestSpeedWSAccountRepo{account: *account}
	svc := &OpenAIGatewayService{accountRepo: repo}
	require.Nil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"))
	repo.revokeMapping()
	require.NotNil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-6.1-sol"), "a stale live socket must not preserve revoked eligibility")
	require.Nil(t, svc.validateFixedRequestSpeedAccount(context.Background(), account, "gpt-4o"), "ordinary context remains unchanged")
	require.NotNil(t, svc.validateFixedRequestSpeedAccount(ctx, account, "gpt-4o"))
	disabled := *account
	disabled.Schedulable = false
	require.NotNil(t, (&OpenAIGatewayService{}).validateFixedRequestSpeedAccount(ctx, &disabled, "gpt-6.1-sol"), "direct callers cannot use an unschedulable fixed account")
}

func TestRequestSpeedWSPassthroughTurnCannotOverrideOrOutliveMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(WithRequestSpeed(context.Background(), 71, RequestSpeedUltrafast))
	defer cancel()
	account := fixedSpeedWSAccount(RequestSpeedUltrafast)
	account.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}
	// The existing lifecycle harness passes this fake token to the service.
	account.Credentials["api_key"] = "sk-test"
	repo := &requestSpeedWSAccountRepo{account: *account}
	upstream := newStagedPassthroughConn()
	svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
	svc.accountRepo = repo
	results := make(chan *OpenAIForwardResult, 2)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
			if result != nil && err == nil {
				results <- result
			}
		}}
	})
	defer server.Close()
	dialCtx, stopDial := context.WithTimeout(ctx, 3*time.Second)
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	stopDial()
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	write := func(payload string) {
		writeCtx, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		require.NoError(t, client.Write(writeCtx, coderws.MessageText, []byte(payload)))
	}
	write(`{"type":"response.create","model":"gpt-6.1-sol","stream":true,"service_tier":"priority","input":"hi"}`)
	first := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.False(t, gjson.GetBytes(first, "service_tier").Exists())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_speed_turn1","model":"gpt-6.1-sol","status":"completed","service_tier":"default","usage":{"input_tokens":100,"output_tokens":20}}}`)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	select {
	case result := <-results:
		require.Equal(t, "ultrafast", result.FixedUpstreamRequestTier)
		require.Equal(t, "default", result.UpstreamResponseServiceTier)
	case <-time.After(time.Second):
		t.Fatal("missing first turn result")
	}
	write(`{"type":"session.update","session":{"model":"gpt-6.1-sol","service_tier":"priority"}}`)
	session := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.False(t, gjson.GetBytes(session, "session.service_tier").Exists())
	repo.revokeMapping()
	write(`{"type":"response.create","model":"gpt-6.1-sol","service_tier":"priority","input":"second"}`)
	denied, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "error", gjson.GetBytes(denied, "type").String())
	require.Contains(t, gjson.GetBytes(denied, "error.message").String(), "no longer supports")
	// Relay deliberately drains an in-flight upstream before exit. After the
	// client has observed the rejection, finish that idle fake transport.
	upstream.Fail(io.EOF)
	// Consume the close frame so coder/websocket can finish its handshake.
	_, _ = readPassthroughLifecycleFrame(t, client, 2*time.Second)
	select {
	case err := <-serverErr:
		var closeErr *OpenAIWSClientCloseError
		require.ErrorAs(t, err, &closeErr)
		require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	case <-time.After(3 * time.Second):
		t.Fatal("revoked mapping did not close session")
	}
	select {
	case payload := <-upstream.writes:
		t.Fatalf("revoked mapping forwarded new turn: %s", payload)
	default:
	}
}

func TestRequestSpeedWSHTTPBridgeRejectsWrongAccountAndModelBeforeSend(t *testing.T) {
	for _, test := range []struct{ name, model, accountTier string }{
		{"wrong_tier", "gpt-6.1-sol", RequestSpeedFast},
		{"unsupported_model", "gpt-4o", RequestSpeedUltrafast},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := WithRequestSpeed(context.Background(), 71, RequestSpeedUltrafast)
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":"hi"}`, test.model))
			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, nil, fixedSpeedWSAccount(test.accountTier), "sk-fixed-test", body, len(body), test.model, "", "", "", "", 1, func([]byte) error { return nil })
			require.Error(t, err)
			require.Nil(t, result)
			require.Empty(t, upstream.lastBody)
		})
	}
}

func TestRequestSpeedWSHTTPForwardUsesFixedKeyAndTier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		t.Run(tier, func(t *testing.T) {
			ctx := WithRequestSpeed(context.Background(), 71, tier)
			cfg := newOpenAIWSV2TestConfig()
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_fixed_v2","model":"gpt-6.1-sol","status":"completed","service_tier":"default","output":[],"usage":{"input_tokens":100,"output_tokens":20}}}`)}}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := fixedSpeedWSAccount(tier)
			account.Extra = map[string]any{"responses_websockets_v2_enabled": true}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6.1-sol","stream":false,"service_tier":"flex","input":[{"type":"input_text","text":"hi"}]}`))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			_, present := capture.lastWrite["service_tier"]
			require.False(t, present)
			require.Equal(t, RequestSpeedServiceTierFromContext(ctx), result.FixedUpstreamRequestTier)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
		})
	}
}

func TestRequestSpeedWSUnknownContinuationFailsClosed(t *testing.T) {
	ctx := WithRequestSpeed(context.Background(), 71, RequestSpeedFast)
	groupID := int64(71)
	for _, test := range []struct {
		name    string
		service *OpenAIGatewayService
	}{
		{"missing_store", &OpenAIGatewayService{}},
		{"missing_binding", &OpenAIGatewayService{cache: &stubGatewayCache{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorIs(t, test.service.ValidatePreviousResponseRequestSpeed(ctx, &groupID, "resp_expired_or_unknown"), ErrRequestSpeedContinuationMismatch)
			require.NoError(t, test.service.ValidatePreviousResponseRequestSpeed(context.Background(), &groupID, "resp_expired_or_unknown"), "legacy groups keep unknown-continuation behavior")
			require.NoError(t, test.service.ValidatePreviousResponseRequestSpeed(ctx, &groupID, ""), "a fresh conversation needs no response binding")
		})
	}
}
