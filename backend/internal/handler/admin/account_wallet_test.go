package admin

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

type walletChangingAdmin struct {
	service.AdminService
	calls int
}

func (a *walletChangingAdmin) GetAccountsByIDs(context.Context, []int64) ([]*service.Account, error) {
	a.calls++
	return []*service.Account{{ID: 1, Type: "oauth", Credentials: map[string]any{"api_key": a.calls}}}, nil
}
func TestAutomaticWalletInputBoundsAndAccountChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &walletChangingAdmin{}
	handler := &AccountHandler{adminService: fake, automaticWallet: service.NewAccountWalletReader(nil)}
	r := gin.New()
	r.GET("/wallet", handler.GetAutomaticWallet)
	for _, q := range []string{"", "id=0", "id=01", "id=-1", "id=1&refresh=yes"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/wallet?"+q, nil))
		require.Equal(t, 400, w.Code)
	}
	require.Zero(t, fake.calls)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/wallet?id=1", nil))
	require.Equal(t, 409, w.Code)
	require.NotContains(t, w.Body.String(), "api_key")
}
