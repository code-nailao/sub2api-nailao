//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type chatToolFilterUpstream struct {
	service.HTTPUpstream
	calls    int
	onSubmit func(*http.Request)
}

func (u *chatToolFilterUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	if u.onSubmit != nil {
		u.onSubmit(req)
	}
	return nil, io.ErrUnexpectedEOF
}

func newDeepSeekChatFilterHandler(t *testing.T, acquireUserSlot func() bool, settings service.SettingRepository, onAccountAcquired func()) (*OpenAIGatewayHandler, *service.APIKey, *chatToolFilterUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	group := &service.Group{ID: 1, Platform: service.PlatformDeepseek, Status: service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
	account := service.Account{
		ID: 1, Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{group.ID}, Concurrency: 1,
		Credentials: map[string]any{"api_key": "fixture-only", "api_protocol": "chat_completions"},
	}
	upstream := &chatToolFilterUpstream{}
	repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	slots := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return acquireUserSlot(), nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			if onAccountAcquired != nil {
				onAccountAcquired()
			}
			return true, nil
		},
	})
	var rate *service.RateLimitService
	if settings != nil {
		rate = service.NewRateLimitService(repo, nil, cfg, nil, nil)
		rate.SetSettingService(service.NewSettingService(settings, cfg))
	}
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, slots,
		service.NewBillingService(cfg, nil), rate, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, slots, billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	h.concurrencyHelper = NewConcurrencyHelper(slots, SSEPingFormatComment, time.Millisecond)
	user := &service.User{ID: 1, Status: service.StatusActive}
	key := &service.APIKey{ID: 1, UserID: user.ID, User: user, GroupID: &group.ID, Group: group}
	return h, key, upstream
}

func TestDeepSeekChatFilterWritesSSEErrorAfterQueueHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	h, key, upstream := newDeepSeekChatFilterHandler(t, func() bool { return rec.Flushed }, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _ := gin.CreateTestContext(rec)
	const body = `{"model":"deepseek-flash","messages":[],"stream":true,"tools":[],"tools":[{"type":"web_search"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), key)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID, Concurrency: 1})
	h.ChatCompletions(c)

	require.True(t, rec.Flushed)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.True(t, strings.HasPrefix(rec.Body.String(), ":\n\n"))
	var errorFrames []gjson.Result
	for _, frame := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		if strings.HasPrefix(frame, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(frame, "data: ")
		require.True(t, ok, "the validation failure must be an SSE data frame")
		require.True(t, gjson.Valid(payload))
		event := gjson.Parse(payload)
		require.Equal(t, "invalid_request_error", event.Get("error.type").String())
		require.Equal(t, "duplicate top-level JSON key: tools", event.Get("error.message").String())
		require.False(t, event.Get("choices").Exists())
		errorFrames = append(errorFrames, event)
	}
	require.Len(t, errorFrames, 1)
	require.Zero(t, upstream.calls)
}

type chatForwardCheckCount struct {
	context.Context
	watching atomic.Bool
	checks   atomic.Int64
}

func (c *chatForwardCheckCount) Err() error {
	if c.watching.Load() {
		c.checks.Add(1)
	}
	return c.Context.Err()
}

func TestDeepSeekChatReusesIngressScan(t *testing.T) {
	// The forwarding scanner checks cancellation every few KiB. Once the body
	// has passed ingress, unrelated metadata must not add that work again.
	forwardChecks := func(padding int) int64 {
		t.Helper()
		probe := &chatForwardCheckCount{Context: t.Context()}
		h, key, upstream := newDeepSeekChatFilterHandler(t, func() bool { return true }, nil, func() { probe.watching.Store(true) })
		const tools = `[{"type":"function","function":{"name":"local_lookup","parameters":{"type":"object"}}}]`
		upstream.onSubmit = func(req *http.Request) {
			probe.watching.Store(false)
			sent, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.JSONEq(t, tools, gjson.GetBytes(sent, "tools").Raw)
		}
		body := `{"model":"deepseek-flash","messages":[{"role":"user","content":"fixture"}],"metadata":{"padding":"` +
			strings.Repeat("x", padding) + `"},"tools":` + tools + `}`
		rec := serveIndexedDeepSeekChat(probe, h, key, body)
		require.Equal(t, 1, upstream.calls, rec.Body.String())
		return probe.checks.Load()
	}

	small, large := forwardChecks(16), forwardChecks(1<<20)
	t.Logf("context checks before upstream submission: small=%d, large=%d", small, large)
	require.LessOrEqual(t, large, small+8, "forwarding must not scan unrelated padding after ingress")
}

func serveIndexedDeepSeekChat(ctx context.Context, h *OpenAIGatewayHandler, key *service.APIKey, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID, Concurrency: 1})
		c.Next()
	}, middleware2.RejectConflictingRequestFields(), h.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestDeepSeekChatFilterWithIndexedFields(t *testing.T) {
	h, key, upstream := newDeepSeekChatFilterHandler(t, func() bool { return true }, nil, nil)
	upstream.onSubmit = func(req *http.Request) {
		sent, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.True(t, gjson.ValidBytes(sent), "%s", sent)
		require.False(t, gjson.GetBytes(sent, "tools").Exists(), "%s", sent)
		require.False(t, gjson.GetBytes(sent, "tool_choice").Exists(), "%s", sent)
		require.Equal(t, "fixture", gjson.GetBytes(sent, "messages.0.content").String())
		require.Equal(t, "system", gjson.GetBytes(sent, "messages.1.role").String())
		require.NotEmpty(t, gjson.GetBytes(sent, "messages.1.content").String())
		require.Equal(t, "keep", gjson.GetBytes(sent, "metadata.other").String())
	}
	const body = `{"\u0074ools":[{"type":"web_search"}], "tool_choice":"required", "model":"deepseek-flash", "messages":[{"role":"user","content":"fixture"}], "metadata":{"other":"keep"}}`
	rec := serveIndexedDeepSeekChat(t.Context(), h, key, body)
	require.Equal(t, 1, upstream.calls, rec.Body.String())
}

type chatToolFilterSchedulerSettings struct{ service.SettingRepository }

func (chatToolFilterSchedulerSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{"openai_advanced_scheduler_enabled": "true"}, nil
}

func (chatToolFilterSchedulerSettings) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func TestDeepSeekChatFilterRejectsWithoutAccountFailure(t *testing.T) {
	// The advanced scheduler setting is cached process-wide. Run this case
	// in isolation so other handler tests cannot mask it or inherit it.
	const child = "SUB2API_TEST_DEEPSEEK_CHAT_SCHEDULER_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestDeepSeekChatFilterRejectsWithoutAccountFailure$")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return
	}

	t.Run("duplicate_tools", func(t *testing.T) {
		h, key, upstream := newDeepSeekChatFilterHandler(t, func() bool { return true }, chatToolFilterSchedulerSettings{}, nil)
		const body = `{"model":"deepseek-flash","messages":[],"tools":[],"tools":[{"type":"web_search"}]}`
		rec := serveRejectionHTTPBody(t, h, key, "chat/completions", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
		requireNoDeepSeekChatAccountFailure(t, h, upstream)
	})

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			acquired := false
			h, key, upstream := newDeepSeekChatFilterHandler(t, func() bool { return true }, chatToolFilterSchedulerSettings{}, func() {
				acquired = true
				cancel(cause)
			})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			const body = `{"model":"deepseek-flash","messages":[],"tools":[{"type":"web_search"}]}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(chatToolFilterContext{ctx})
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware2.ContextKeyAPIKey), key)
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID, Concurrency: 1})
			h.ChatCompletions(c)

			require.True(t, acquired, "cancellation must happen after the account slot is granted")
			require.ErrorIs(t, c.Request.Context().Err(), cause)
			requireNoDeepSeekChatAccountFailure(t, h, upstream)
		})
	}
}

// Report the simulated deadline at slot acquisition without relying on a timer.
type chatToolFilterContext struct{ context.Context }

func (c chatToolFilterContext) Err() error { return context.Cause(c.Context) }

func requireNoDeepSeekChatAccountFailure(t *testing.T, h *OpenAIGatewayHandler, upstream *chatToolFilterUpstream) {
	t.Helper()
	require.Zero(t, upstream.calls)
	metrics := h.gatewayService.SnapshotOpenAIAccountSchedulerMetrics()
	require.Positive(t, metrics.SelectTotal, "advanced scheduling must have selected the account")
	require.Zero(t, metrics.RuntimeStatsAccountCount, "a local rejection must not create an account result sample")
}
