//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestSpeedAuthRepo struct {
	*stubApiKeyRepo
	current *service.APIKey
	reads   int
}

func (r *requestSpeedAuthRepo) GetByID(context.Context, int64) (*service.APIKey, error) {
	r.reads++
	return r.current, nil
}

func TestAPIKeyRequestSpeedAuthFreezesAndRevalidatesNativeConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"unchanged", "tier_changed", "group_disabled", "key_disabled", "group_switched", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			ultra := "ultrafast"
			fast := "fast"
			gid := int64(91)
			group := &service.Group{ID: gid, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, SubscriptionType: service.SubscriptionTypeStandard, RequestSpeedSelectionEnabled: mode != "legacy", DefaultRequestSpeedTier: ultra}
			user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 3}
			key := &service.APIKey{ID: 9, Key: "sk-local-speed", UserID: user.ID, User: user, Status: service.StatusActive, GroupID: &gid, Group: group, RequestSpeedTier: &fast}
			repo := &requestSpeedAuthRepo{stubApiKeyRepo: &stubApiKeyRepo{}, current: key}
			repo.getByKey = func(context.Context, string) (*service.APIKey, error) { return key, nil }
			cfg := &config.Config{RunMode: config.RunModeSimple}
			keySvc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			r := gin.New()
			r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keySvc, nil, cfg)))
			r.GET("/t", func(c *gin.Context) {
				if mode == "legacy" {
					require.Empty(t, service.RequestSpeedFromContext(c.Request.Context()))
					require.Nil(t, service.ValidateRequestSpeedConfiguration(c.Request.Context()))
					require.Zero(t, repo.reads)
					c.Status(200)
					return
				}
				require.Equal(t, fast, service.RequestSpeedFromContext(c.Request.Context()))
				require.Equal(t, gid, service.RequestSpeedGroupIDFromContext(c.Request.Context()))
				require.Nil(t, service.ValidateRequestSpeedConfiguration(c.Request.Context()))
				latest := *key
				latestGroup := *group
				latest.Group = &latestGroup
				switch mode {
				case "tier_changed":
					latest.RequestSpeedTier = &ultra
				case "group_disabled":
					latestGroup.RequestSpeedSelectionEnabled = false
				case "key_disabled":
					latest.Status = "disabled"
				case "group_switched":
					newID := int64(92)
					latest.GroupID = &newID
					latestGroup.ID = newID
				}
				repo.current = &latest
				blocked := service.ValidateRequestSpeedConfiguration(c.Request.Context())
				if mode == "unchanged" {
					require.Nil(t, blocked)
				} else {
					require.NotNil(t, blocked)
					require.Contains(t, blocked.Message, "new connection")
				}
				require.Equal(t, fast, service.RequestSpeedFromContext(c.Request.Context()), "a changed Key must not silently change a live request tier")
				c.Status(200)
			})
			req := httptest.NewRequest(http.MethodGet, "/t", nil)
			req.Header.Set("x-api-key", key.Key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
		})
	}
}
