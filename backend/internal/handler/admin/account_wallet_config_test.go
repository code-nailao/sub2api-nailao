package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type walletConfigAdmin struct {
	service.AdminService
	account *service.Account
	calls   int
}

func (s *walletConfigAdmin) GetAccountsByIDs(context.Context, []int64) ([]*service.Account, error) {
	s.calls++
	return []*service.Account{s.account}, nil
}

func TestWalletConfigHandlerDoesNotExposeCredentialsAndBoundsInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &walletConfigAdmin{account: &service.Account{ID: 1, Platform: "openai", Type: "apikey", Credentials: map[string]any{
		"api_key": "private-key", "base_url": "https://user:secret@direct.example/v1?token=secret#fragment"}}}
	h := &AccountHandler{adminService: fake, automaticWallet: service.NewAccountWalletReader(nil)}
	r := gin.New()
	r.GET("/config", h.GetWalletConfiguration)
	r.POST("/config/test", h.TestWalletConfiguration)
	r.PUT("/config", h.SaveWalletConfiguration)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config?id=1", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "https://direct.example/v1")
	for _, secret := range []string{"private-key", "secret", "fragment", "api_key"} {
		require.NotContains(t, w.Body.String(), secret)
	}
	for _, id := range []string{"", "0", "01", "-1", "1.0"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config?id="+id, nil))
		require.Equal(t, 400, w.Code)
	}
	before := fake.calls
	for _, body := range []string{`{}`, `{`, `{"url":"https://example.com","revision":"short"}`, `{"revision":"` + strings.Repeat("a", 64) + `","api_key":"injected"}`, strings.Repeat(" ", 4097) + `{}`} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/config/test?id=1", strings.NewReader(body)))
		require.Equal(t, 400, w.Code)
	}
	require.Equal(t, before, fake.calls)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/config?id=1", strings.NewReader(`{"url":"https://example.com","revision":"`+strings.Repeat("a", 64)+`"}`)))
	require.Equal(t, 409, w.Code)
	require.Contains(t, w.Body.String(), "wallet_config_changed")
}
