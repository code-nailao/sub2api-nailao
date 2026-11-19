//go:build embed

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedFrontendPreservesWalletBridgeHandlerAndAuthentication(t *testing.T) {
	server, err := NewFrontendServer(&mockSettingsProvider{settings: map[string]string{"site_name": "Test"}})
	require.NoError(t, err)
	for name, handler := range map[string]gin.HandlerFunc{
		"settings_injection": server.Middleware(),
		"legacy":             ServeEmbeddedFrontend(),
	} {
		t.Run(name, func(t *testing.T) {
			router := gin.New()
			router.Use(handler)
			calls := 0
			// Match production's ordering: internal API registration happens
			// after the embedded SPA middleware, with its own scoped auth.
			router.POST("/internal/sources-wallet/snapshot", func(c *gin.Context) {
				calls++
				if c.GetHeader("Authorization") != "Bearer synthetic-bridge-capability" {
					c.Status(http.StatusNotFound)
					return
				}
				c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"synchronized": true}})
			})
			for _, authorization := range []string{"", "Bearer wrong", "Bearer synthetic-bridge-capability"} {
				request := httptest.NewRequest(http.MethodPost, "/internal/sources-wallet/snapshot", strings.NewReader(`{}`))
				request.Header.Set("Authorization", authorization)
				request.Header.Set("Content-Type", "application/json")
				result := httptest.NewRecorder()
				router.ServeHTTP(result, request)
				if authorization == "Bearer synthetic-bridge-capability" {
					require.Equal(t, http.StatusOK, result.Code)
					require.Contains(t, result.Header().Get("Content-Type"), "application/json")
					require.JSONEq(t, `{"code":0,"data":{"synchronized":true}}`, result.Body.String())
				} else {
					require.Equal(t, http.StatusNotFound, result.Code)
					require.Empty(t, result.Body.String())
				}
			}
			require.Equal(t, 3, calls, "SPA middleware must not replace authenticated or rejected API responses")

			wrongMethod := httptest.NewRecorder()
			router.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodGet, "/internal/sources-wallet/snapshot", nil))
			require.Equal(t, http.StatusNotFound, wrongMethod.Code)
			require.NotContains(t, wrongMethod.Header().Get("Content-Type"), "text/html")

			page := httptest.NewRecorder()
			router.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
			require.Equal(t, http.StatusOK, page.Code)
			require.Contains(t, page.Header().Get("Content-Type"), "text/html")
			require.Contains(t, strings.ToLower(page.Body.String()), "<html")
		})
	}
}
