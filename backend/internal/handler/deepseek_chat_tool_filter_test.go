//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	calls int
}

func (u *chatToolFilterUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls++
	return nil, io.ErrUnexpectedEOF
}

func TestDeepSeekChatFilterWritesSSEErrorAfterQueueHeartbeat(t *testing.T) {
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
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	slots := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return rec.Flushed, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	})
	h := NewOpenAIGatewayHandler(gateway, slots, billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	h.concurrencyHelper = NewConcurrencyHelper(slots, SSEPingFormatComment, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	user := &service.User{ID: 1, Status: service.StatusActive}
	key := &service.APIKey{ID: 1, UserID: user.ID, User: user, GroupID: &group.ID, Group: group}
	c, _ := gin.CreateTestContext(rec)
	const body = `{"model":"deepseek-flash","messages":[],"stream":true,"tools":[],"tools":[{"type":"web_search"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), key)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID, Concurrency: 1})
	h.ChatCompletions(c)

	require.Equal(t, http.StatusOK, rec.Code, "the heartbeat commits the response headers")
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.True(t, strings.HasPrefix(rec.Body.String(), ":\n\n"))
	var events []string
	for _, frame := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		if strings.HasPrefix(frame, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(frame, "data: ")
		require.True(t, ok, "every non-heartbeat frame must be SSE data")
		require.True(t, gjson.Valid(payload))
		events = append(events, payload)
	}
	require.Len(t, events, 1)
	require.Equal(t, "invalid_request_error", gjson.Get(events[0], "error.type").String())
	require.Contains(t, gjson.Get(events[0], "error.message").String(), "duplicate top-level JSON key: tools")
	require.Zero(t, upstream.calls)
}
