//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestIsGrokVideoTaskRead(t *testing.T) {
	for _, prefix := range []string{"/videos/", "/v1/videos/"} {
		for _, shape := range []string{"task_1", "task_1/content", "generations/task_1", "generations/task_1/content", "edits/task_1", "extensions/task_1/content"} {
			require.True(t, isGrokVideoTaskRead(http.MethodGet, prefix+shape), prefix+shape)
			require.False(t, isGrokVideoTaskRead(http.MethodPost, prefix+shape), prefix+shape)
		}
	}
	for _, path := range []string{"/v1/videos", "/v1/videos/", "/v1/videos/generations", "/v1/videos/edits", "/v1/videos/extensions", "/v1/videos//content", "/v1/videos/task/unknown", "/v1/videos/task/content/extra", "/v1/videos/../content", "/v1/videos/%2fcontent", "/other/videos/task", "/v1bad/videos/task", "/v1/videos/task/"} {
		require.False(t, isGrokVideoTaskRead(http.MethodGet, path), path)
	}
}

func TestGrokVideoTaskReadSkipsFundsButKeepsAuthGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		alter  func(*service.APIKey)
		want   int
	}{
		{"status_after_exhaustion", http.MethodGet, "/v1/videos/task_1", nil, 200},
		{"content_after_exhaustion", http.MethodGet, "/v1/videos/task_1/content", nil, 200},
		{"alias_after_exhaustion", http.MethodGet, "/videos/generations/task_1/content", nil, 200},
		{"new_generation_denied", http.MethodPost, "/v1/videos/generations", nil, 429},
		{"disabled_key_denied", http.MethodGet, "/v1/videos/task_1", func(k *service.APIKey) { k.Status = "inactive" }, 401},
		{"disabled_user_denied", http.MethodGet, "/v1/videos/task_1", func(k *service.APIKey) { k.User.Status = "disabled" }, 401},
		{"disabled_group_denied", http.MethodGet, "/v1/videos/task_1", func(k *service.APIKey) { k.Group.Status = "inactive" }, 403},
		{"exclusive_group_revoked", http.MethodGet, "/v1/videos/task_1", func(k *service.APIKey) { k.Group.IsExclusive = true }, 403},
		{"expired_credential_denied", http.MethodGet, "/v1/videos/task_1", func(k *service.APIKey) { past := time.Now().Add(-time.Hour); k.ExpiresAt = &past }, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 22, Platform: service.PlatformGrok, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, Hydrated: true}
			key := &service.APIKey{ID: 3, UserID: 7, Key: "video-task-fixture", Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1, GroupID: &group.ID, Group: group, User: &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 0}}
			if tc.alter != nil {
				tc.alter(key)
			}
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			router := gin.New()
			router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
			router.Handle(tc.method, tc.path, func(c *gin.Context) {
				// Successful auth preserves identity for the media owner's check.
				got, ok := GetAPIKeyFromContext(c)
				require.True(t, ok)
				require.Equal(t, key.ID, got.ID)
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set("Authorization", "Bearer "+key.Key)
			out := httptest.NewRecorder()
			router.ServeHTTP(out, request)
			require.Equal(t, tc.want, out.Code, out.Body.String())
		})
	}
}
