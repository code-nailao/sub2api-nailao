package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pendingChargeRouteReader struct {
	service.UsageBillingRepository
	owner  int64
	called bool
}

func (r *pendingChargeRouteReader) ListUserPendingUpstreamRejections(_ context.Context, owner int64, _ string, _ int) (*service.UserPendingUpstreamRejectionsPage, error) {
	r.owner, r.called = owner, true
	return &service.UserPendingUpstreamRejectionsPage{}, nil
}

func TestPendingChargesUserRouteAuthenticatedStaticAndReadOnly(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		reader := &pendingChargeRouteReader{}
		h := &handler.Handlers{UpstreamRejection: handler.NewUpstreamRejectionHandler(reader)}
		jwt := middleware.JWTAuthMiddleware(func(c *gin.Context) {
			if !authenticated {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
			c.Next()
		})
		audit := middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
		RegisterUserRoutes(r.Group("/api/v1"), h, jwt, audit, nil, nil)
		for _, query := range []string{"", "?timezone=Asia%2FShanghai", "?timezone=UTC", "?timezone=Invalid%2FZone", "?timezone=UTC&user_id=43"} {
			reader.called = false
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/pending-charges"+query, nil))
			if !authenticated {
				require.Equal(t, http.StatusUnauthorized, rec.Code)
				require.False(t, reader.called)
			} else if query == "?timezone=Invalid%2FZone" || query == "?timezone=UTC&user_id=43" {
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.False(t, reader.called)
			} else {
				require.Equal(t, http.StatusOK, rec.Code, "static pending path must not become usage/:id")
				require.True(t, reader.called)
				require.Equal(t, int64(42), reader.owner)
			}
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/usage/pending-charges", nil))
			require.Equal(t, http.StatusNotFound, rec.Code, "no refund/resolve mutation route may be exposed")
		}
	}
}
