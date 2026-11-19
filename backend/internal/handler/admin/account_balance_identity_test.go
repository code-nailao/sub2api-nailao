package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBalanceMarkerOnlyTracksCallingIdentity(t *testing.T) {
	a := &service.Account{ID: 42, Platform: "openai", Type: "apikey", CreatedAt: time.Unix(100, 0), Credentials: map[string]any{"api_key": "model-secret", "base_url": "https://models.example/v1"}}
	secret := strings.Repeat("private-signing-key", 3)
	first := balanceIdentity(a, secret)
	require.True(t, first.Supported)
	require.Len(t, first.Marker, 67)
	a.Name = "new name"
	a.Priority = 20
	a.UpdatedAt = time.Now()
	a.Extra = map[string]any{"note": "new"}
	require.Equal(t, first, balanceIdentity(a, secret))
	a.Credentials["api_key"] = "another-secret"
	require.NotEqual(t, first.Marker, balanceIdentity(a, secret).Marker)
	a.Credentials["api_key"] = "model-secret"
	a.Credentials["base_url"] = "https://other.example/v1"
	require.NotEqual(t, first.Marker, balanceIdentity(a, secret).Marker)
	a.Credentials["base_url"] = "https://models.example/v1"
	require.NotEqual(t, first.Marker, balanceIdentity(a, "another-signing-key").Marker)
	raw, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "model-secret")
	require.NotContains(t, string(raw), "private-signing")
	a.Type = "oauth"
	require.False(t, balanceIdentity(a, secret).Supported)
	a.Type = "apikey"
	a.Platform = "antigravity"
	require.False(t, balanceIdentity(a, secret).Supported)
}

type balanceIdentityAdmin struct {
	service.AdminService
	accounts []*service.Account
	ids      []int64
}

func (a *balanceIdentityAdmin) GetAccountsByIDs(_ context.Context, ids []int64) ([]*service.Account, error) {
	a.ids = ids
	return a.accounts, nil
}
func TestBalanceIdentityEndpointBoundsAndReturnsNoCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &balanceIdentityAdmin{accounts: []*service.Account{{ID: 1, Type: "apikey", Platform: "openai", Credentials: map[string]any{"api_key": "upstream-secret", "base_url": "https://up.example"}}}}
	handler := &AccountHandler{adminService: fake, cfg: &config.Config{JWT: config.JWTConfig{Secret: strings.Repeat("x", 32)}}}
	r := gin.New()
	r.GET("/identities", handler.GetBalanceIdentities)
	for _, q := range []string{"", "0", "1,1", "01", "1,-2", strings.Repeat("1,", 100) + "2"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/identities?ids="+q, nil))
		require.Equal(t, 400, w.Code, q)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/identities?ids=1,2", nil))
	require.Equal(t, 200, w.Code)
	require.Equal(t, []int64{1, 2}, fake.ids)
	require.Contains(t, w.Body.String(), `"supported":true`)
	require.NotContains(t, w.Body.String(), "upstream-secret")
	require.NotContains(t, w.Body.String(), "up.example")
}

func TestBalanceMarkerTracksAdaptiveEndpointAndProtocolChanges(t *testing.T) {
	a := &service.Account{ID: 7, Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "private-model-key", "api_protocol": "adaptive", "account_mode": "payg",
		"api_base_urls": map[string]any{"chat_completions": "https://first.example", "anthropic": "https://claude.example"},
	}}
	secret := strings.Repeat("s", 32)
	initial := balanceIdentity(a, secret)
	require.True(t, initial.Supported)
	a.Credentials["api_base_urls"] = map[string]any{"anthropic": "https://claude.example", "chat_completions": "https://first.example"}
	require.Equal(t, initial.Marker, balanceIdentity(a, secret).Marker)
	a.Credentials["api_base_urls"] = map[string]any{"anthropic": "https://changed.example", "chat_completions": "https://first.example"}
	require.NotEqual(t, initial.Marker, balanceIdentity(a, secret).Marker)
	a.Credentials["api_base_urls"] = map[string]any{"anthropic": "https://claude.example", "chat_completions": "https://first.example"}
	a.Credentials["account_mode"] = "coding"
	require.NotEqual(t, initial.Marker, balanceIdentity(a, secret).Marker)
	a.Credentials["account_mode"] = "payg"
	a.Credentials["api_protocol"] = "anthropic"
	require.NotEqual(t, initial.Marker, balanceIdentity(a, secret).Marker)
}

func TestBalanceMarkerTracksOpenCodeMode(t *testing.T) {
	a := &service.Account{ID: 8, Platform: "opencode_go", Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "private-model-key", "account_mode": "go"}}
	secret := strings.Repeat("s", 32)
	initial := balanceIdentity(a, secret)
	a.Credentials["account_mode"] = "zen"
	require.NotEqual(t, initial.Marker, balanceIdentity(a, secret).Marker)
}
