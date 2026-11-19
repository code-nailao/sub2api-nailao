package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekPaidContinuationDoesNotRequestSDKRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h := &OpenAIGatewayHandler{}
	h.writeDeepSeekContinuationError(c, &service.OpenAIForwardResult{RequestID: "deepseek-tools:receipt"})
	// Official SDKs retry 408, 409, 429 and 5xx. A paid partial workflow must
	// return a non-retryable response with its receipt for reconciliation.
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	require.Equal(t, "false", w.Header().Get("x-should-retry"))
	require.Contains(t, w.Body.String(), "deepseek-tools:receipt")
	require.Contains(t, w.Body.String(), "tool_continuation_failed")
}

// Requests the gateway refuses on its own never reach account selection or a
// supplier: this handler has no gateway service, so any later step would panic.
func TestDeepSeekToolEndpointsRefuseBeforeAnySupplierCall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	price := 6.0
	ocrOnly := service.DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: &price, OCRPricePer1KPages: &price}
	for _, tc := range []struct {
		name, path, contentType string
		body                    string
		tools                   service.DeepSeekToolsConfig
		status                  int
		message                 string
	}{
		{"unknown_tool", "/v1/tools/translate", "application/json", `{}`, ocrOnly, http.StatusNotFound, "unsupported DeepSeek tool"},
		{"tool_not_offered", "/v1/tools/tts", "application/json", `{"text":"hi"}`, ocrOnly, http.StatusForbidden, "DeepSeek tts is not offered for this group"},
		{"tools_disabled", "/v1/tools/ocr", "multipart/form-data; boundary=x", `--x--`, service.DeepSeekToolsConfig{OCRPricePer1KPages: &price}, http.StatusForbidden, "disabled"},
		{"not_multipart", "/v1/tools/ocr", "application/json", `{"file":"a"}`, ocrOnly, http.StatusUnsupportedMediaType, "multipart/form-data"},
		{"body_too_large", "/v1/tools/search", "application/json", `{"query":"` + strings.Repeat("a", 1<<20) + `"}`, ocrOnly, http.StatusRequestEntityTooLarge, "too large"},
		{"invalid_search", "/v1/tools/search", "application/json", `{"query":"x","model":"free"}`, ocrOnly, http.StatusBadRequest, "unsupported search field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil)
			t.Cleanup(billing.Stop)
			h := &OpenAIGatewayHandler{billingCacheService: billing}
			router.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, User: &service.User{ID: 1}, Group: &service.Group{Platform: service.PlatformDeepseek, SearchPricePer1k: &price, DeepSeekTools: tc.tools}})
				c.Next()
			})
			router.POST("/v1/tools/:tool", h.DeepSeekTool)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.message)
		})
	}
}
