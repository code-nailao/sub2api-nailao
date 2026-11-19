//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// OCR and ASR keep running after the client disconnects, so their user slot
// must stay taken until the handler returns; other tools free it with the
// request as before.
func TestDeepSeekToolUserSlotOutlivesTheClientForRecognition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		operation string
		heldAfter bool
	}{{service.DeepSeekToolOCR, true}, {service.DeepSeekToolASR, true}, {service.DeepSeekToolTTS, false}, {service.DeepSeekToolSearch, false}} {
		t.Run(tc.operation, func(t *testing.T) {
			slots := &grokMediaSlotsCache{accounts: map[string]int64{}, users: map[string]int64{}}
			h := &OpenAIGatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(slots), SSEPingFormatNone, 0)}
			ctx, cancel := context.WithCancel(context.Background())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/tools/"+tc.operation, nil).WithContext(ctx)

			release, ok := h.acquireDeepSeekToolUserSlot(c, tc.operation, 1, 1, zap.NewNop())
			require.True(t, ok)
			cancel() // the client disconnects while the supplier call runs
			require.Eventually(t, func() bool {
				slots.mu.Lock()
				defer slots.mu.Unlock()
				return (len(slots.users) == 1) == tc.heldAfter
			}, time.Second, 5*time.Millisecond)
			if tc.heldAfter {
				time.Sleep(20 * time.Millisecond)
				slots.mu.Lock()
				require.Len(t, slots.users, 1, "the slot is held until the handler returns")
				slots.mu.Unlock()
			}
			release()
			slots.assertReleased(t)
		})
	}
}

type deepSeekEmptyBalanceCache struct{ service.BillingCache }

func (deepSeekEmptyBalanceCache) GetUserBalance(context.Context, int64) (float64, error) {
	return 0, nil
}

type deepSeekWatchedBody struct{ read bool }

func (b *deepSeekWatchedBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

// A user who cannot pay is refused before the gateway buffers any upload.
func TestDeepSeekToolBillingCheckPrecedesTheUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	billing := service.NewBillingCacheService(deepSeekEmptyBalanceCache{}, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	h := &OpenAIGatewayHandler{billingCacheService: billing}
	price := 6.0
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, User: &service.User{ID: 1}, Group: &service.Group{Platform: service.PlatformDeepseek, SearchPricePer1k: &price,
			DeepSeekTools: service.DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: &price, OCRPricePer1KPages: &price}}})
		c.Next()
	})
	router.POST("/v1/tools/:tool", h.DeepSeekTool)
	body := &deepSeekWatchedBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/ocr", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.False(t, body.read, "the upload is not read for a user who cannot pay")
}
