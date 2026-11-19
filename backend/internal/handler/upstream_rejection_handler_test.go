package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type userPendingChargeReaderFixture struct {
	service.UsageBillingRepository
	called   bool
	owner    int64
	cursor   string
	pageSize int
	err      error
	page     *service.UserPendingUpstreamRejectionsPage
}

func (f *userPendingChargeReaderFixture) ListUserPendingUpstreamRejections(ctx context.Context, owner int64, cursor string, pageSize int) (*service.UserPendingUpstreamRejectionsPage, error) {
	f.called, f.owner, f.cursor, f.pageSize = true, owner, cursor, pageSize
	if f.err != nil {
		return nil, f.err
	}
	if f.page != nil {
		return f.page, nil
	}
	return &service.UserPendingUpstreamRejectionsPage{Items: []service.UserPendingUpstreamRejection{{
		RequestID: fmt.Sprintf("owned-request-%d", owner), Model: "grok-fixture", State: service.UpstreamRejectionUnknown,
		HeldPrice: 0.1, CreatedAt: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
	}}}, nil
}

func pendingChargeHandlerRouter(reader *userPendingChargeReaderFixture, owner int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if owner != 0 {
		r.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: owner})
			c.Next()
		})
	}
	h := NewUpstreamRejectionHandler(reader)
	r.GET("/usage/pending-charges", h.ListPending)
	r.GET("/usage/:id", func(c *gin.Context) { c.Status(http.StatusTeapot) })
	return r
}

func getPendingCharges(r *gin.Engine, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/usage/pending-charges"+query, nil))
	return rec
}

func TestPendingChargesRequireAuthenticatedOwner(t *testing.T) {
	for _, owner := range []int64{0, -1} {
		reader := &userPendingChargeReaderFixture{}
		rec := getPendingCharges(pendingChargeHandlerRouter(reader, owner), "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.False(t, reader.called)
	}
	for _, owner := range []int64{42, 43} {
		reader := &userPendingChargeReaderFixture{}
		r := pendingChargeHandlerRouter(reader, owner)
		rec := getPendingCharges(r, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, owner, reader.owner)
		require.Contains(t, rec.Body.String(), fmt.Sprintf("owned-request-%d", owner))
		require.NotContains(t, rec.Body.String(), fmt.Sprintf("owned-request-%d", 85-owner))
		reader.called = false
		rec = getPendingCharges(r, "?user_id=99")
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.False(t, reader.called, "query parameters cannot override the authenticated owner")
	}
}

func TestPendingChargesBoundedPagination(t *testing.T) {
	for _, query := range []string{"?page_size=0", "?page_size=51", "?page_size=-1", "?page_size=1.5", "?page_size=01", "?page_size=", "?page_size=1&page_size=50", "?cursor=a&cursor=b", "?cursor=" + strings.Repeat("a", 513), "?cursor=%20a", "?account_id=1"} {
		reader := &userPendingChargeReaderFixture{}
		rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), query)
		require.Equal(t, http.StatusBadRequest, rec.Code, query)
		require.False(t, reader.called, query)
	}
	reader := &userPendingChargeReaderFixture{}
	rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 20, reader.pageSize)
	rec = getPendingCharges(pendingChargeHandlerRouter(reader, 42), "?page_size=50&cursor=owner-bound-cursor")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 50, reader.pageSize)
	require.Equal(t, "owner-bound-cursor", reader.cursor)

	reader.err = service.ErrUpstreamRejectionInvalid
	rec = getPendingCharges(pendingChargeHandlerRouter(reader, 42), "?cursor=other-owner-cursor")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPendingChargesFieldWhitelistAndEmptyPage(t *testing.T) {
	reader := &userPendingChargeReaderFixture{}
	rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), "")
	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data, 3)
	require.Nil(t, envelope.Data["next_cursor"])
	require.Equal(t, float64(20), envelope.Data["page_size"])
	items, ok := envelope.Data["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	require.Len(t, item, 5)
	for _, name := range []string{"request_id", "model", "state", "held_price", "created_at"} {
		require.Contains(t, item, name)
	}
	for _, name := range []string{"user_id", "api_key_id", "account_id", "upstream_cost", "upstream_currency", "payload", "request_payload_hash", "provider_profile"} {
		require.NotContains(t, item, name)
	}
	require.Equal(t, 0.1, item["held_price"])

	reader.page = &service.UserPendingUpstreamRejectionsPage{}
	rec = getPendingCharges(pendingChargeHandlerRouter(reader, 42), "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"items":[],"next_cursor":null,"page_size":20}}`, rec.Body.String())
	reader.page.NextCursor = "next-page"
	rec = getPendingCharges(pendingChargeHandlerRouter(reader, 42), "?page_size=1")
	require.Contains(t, rec.Body.String(), `"next_cursor":"next-page"`)
}

func TestPendingChargesTimezoneDisplayParameterDoesNotChangeQuery(t *testing.T) {
	for _, zone := range []string{"Asia%2FShanghai", "UTC"} {
		reader := &userPendingChargeReaderFixture{}
		rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), "?page_size=1&cursor=owned-position&timezone="+zone)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, int64(42), reader.owner)
		require.Equal(t, "owned-position", reader.cursor)
		require.Equal(t, 1, reader.pageSize)
		require.Contains(t, rec.Body.String(), `"held_price":0.1`)
		require.Contains(t, rec.Body.String(), `"created_at":"2026-11-03T00:00:00Z"`)
	}
	for _, query := range []string{"?timezone=", "?timezone=not-a-timezone", "?timezone=Local", "?timezone=%20UTC", "?timezone=" + strings.Repeat("x", 129), "?timezone=UTC&timezone=Asia%2FShanghai", "?timezone=UTC&user_id=43"} {
		reader := &userPendingChargeReaderFixture{}
		rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), query)
		require.Equal(t, http.StatusBadRequest, rec.Code, query)
		require.False(t, reader.called)
	}
}

func TestPendingChargesReadFailureNeverMasqueradesAsNoHolds(t *testing.T) {
	reader := &userPendingChargeReaderFixture{err: errors.New("database detail with private supplier identity")}
	rec := getPendingCharges(pendingChargeHandlerRouter(reader, 42), "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotContains(t, rec.Body.String(), "private supplier identity")
	require.NotContains(t, rec.Body.String(), `"items":[]`)
	reader.err = nil
	for _, state := range []string{service.UpstreamRejectionNormal, service.UpstreamRejectionReleased, service.UpstreamRejectionRejected} {
		reader.page = &service.UserPendingUpstreamRejectionsPage{Items: []service.UserPendingUpstreamRejection{{State: state, HeldPrice: 0.1}}}
		rec = getPendingCharges(pendingChargeHandlerRouter(reader, 42), "")
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, state)
	}
}
