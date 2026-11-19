package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func (h *OpenAIGatewayHandler) setDeepSeekUsageMetadata(c *gin.Context, key *service.APIKey, account *service.Account, fields service.ChannelUsageFields, pricingAt time.Time, body []byte) {
	service.SetDeepSeekBillingInput(c, &service.OpenAIRecordUsageInput{
		APIKey: key, User: key.User, Account: account, InboundEndpoint: GetInboundEndpoint(c),
		UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c), SessionID: service.ExtractClientSessionID(c),
		RequestPayloadHash: service.HashUsageRequestPayload(body), APIKeyService: h.apiKeyService,
		QuotaPlatform: service.QuotaPlatform(c.Request.Context(), key), ChannelUsageFields: fields, PricingAt: pricingAt,
	})
}

func (h *OpenAIGatewayHandler) writeDeepSeekContinuationError(c *gin.Context, result *service.OpenAIForwardResult) {
	if c.Writer.Written() {
		return
	}
	// OpenAI SDKs retry 409 as well as 5xx. Use a non-retryable status and
	// explicit retry header after confirmed usage to avoid replaying paid work.
	c.Header("x-should-retry", "false")
	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"type": "tool_continuation_failed", "message": "The continuation failed after confirmed usage. Do not automatically replay the request.", "request_id": result.RequestID}})
}

// DeepSeekTool shares provider execution and billing with hosted tools. It does
// not accept a model and never enters token or model fallback pricing.
func (h *OpenAIGatewayHandler) DeepSeekTool(c *gin.Context) {
	started := time.Now()
	operation := strings.TrimPrefix(c.Request.URL.Path, "/v1/tools/")
	fail := func(status int, message string) {
		c.JSON(status, gin.H{"error": gin.H{"type": "deepseek_tool_error", "message": message}})
	}
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		fail(http.StatusUnauthorized, "API key required")
		return
	}
	if key.Group == nil || key.Group.Platform != service.PlatformDeepseek || !key.Group.DeepSeekTools.Enabled {
		fail(http.StatusForbidden, "DeepSeek tools are disabled for this group")
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		fail(http.StatusBadRequest, "invalid request body")
		return
	}
	body, err = service.ValidateDeepSeekToolRequest(operation, body)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		fail(http.StatusUnauthorized, "user required")
		return
	}
	query := gjson.GetBytes(body, "query").String()
	if operation == "fetch" {
		query = gjson.GetBytes(body, "url").String()
	}
	auditBody, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": query}}})
	if decision := h.checkSecurityAudit(c, requestLogger(c, "handler.deepseek.tools"), key, subject, service.ContentModerationProtocolOpenAIChat, "deepseek-tools-"+operation, auditBody); decision != nil && !decision.AllowNextStage {
		status := decision.HTTPStatus
		if status == 0 {
			status = http.StatusForbidden
		}
		fail(status, decision.ClientMessage)
		return
	}
	streamStarted := false
	release, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, requestLogger(c, "handler.deepseek.tools"))
	if !acquired {
		return
	}
	if release != nil {
		defer release()
	}
	if err = h.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, nil, service.PlatformDeepseek); err != nil {
		status, _, message, _ := billingErrorDetails(err)
		fail(status, message)
		return
	}
	account, usage, release, err := h.gatewayService.PrepareDeepSeekTool(c.Request.Context(), key)
	if err != nil {
		fail(http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()
	price := usage.SearchPrice
	if operation == "fetch" {
		price = usage.FetchPrice
	}
	if key.User.Balance < price || (key.Quota > 0 && key.Quota-key.QuotaUsed < price) {
		fail(http.StatusPaymentRequired, "insufficient balance or API key quota for tool operation")
		return
	}
	reservation, err := h.billingCacheService.ReserveFixedCharge(c.Request.Context(), key.User, key, key.Group, nil, price)
	if err != nil {
		status, _, message, _ := billingErrorDetails(err)
		fail(status, message)
		return
	}
	if reservation != nil {
		defer reservation.HandlerDone()
		c.Request = c.Request.WithContext(service.WithInflightReservation(c.Request.Context(), reservation))
	}
	result := &service.OpenAIForwardResult{RequestID: "deepseek-tools:" + uuid.NewString(), Model: "deepseek-tools-" + operation, DeepSeekTools: usage}
	input := &service.OpenAIRecordUsageInput{Result: result, APIKey: key, User: key.User, Account: account, InboundEndpoint: c.Request.URL.Path, UpstreamEndpoint: "/v1/tools/" + operation, UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c), RequestPayloadHash: service.HashUsageRequestPayload(body), APIKeyService: h.apiKeyService, QuotaPlatform: service.PlatformDeepseek, PricingAt: started}
	if err = h.gatewayService.BeginDeepSeekToolUsage(c.Request.Context(), input); err != nil {
		fail(http.StatusServiceUnavailable, "tool billing journal unavailable")
		return
	}
	c.Header("X-Sources-Request-Id", result.RequestID)
	data, success, err := h.gatewayService.ExecuteDeepSeekTool(c.Request.Context(), account, operation, body)
	if err != nil {
		fail(http.StatusBadGateway, err.Error())
		return
	}
	if success {
		if operation == "search" {
			usage.SearchCount = 1
		} else {
			usage.FetchCount = 1
		}
	}
	result.Duration = time.Since(started)
	// Settle before returning a successful standalone response. A disconnect
	// cannot cancel accounting for an already completed supplier operation.
	billingCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
	defer cancel()
	if err = h.gatewayService.RecordUsage(billingCtx, input); err != nil {
		c.Header("x-should-retry", "false")
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"type": "tool_billing_pending", "message": "Tool completed but billing could not be confirmed; do not automatically retry.", "request_id": result.RequestID}})
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}
