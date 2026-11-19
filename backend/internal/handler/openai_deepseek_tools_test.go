package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
