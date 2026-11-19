package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
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

// DeepSeekTool serves the five standalone tools (search, fetch, ocr, asr, tts)
// as thin relays. It shares provider execution and billing with hosted tools,
// does not accept a model and never enters token or model fallback pricing.
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
	if !service.IsDeepSeekToolOperation(operation) {
		fail(http.StatusNotFound, "unsupported DeepSeek tool")
		return
	}
	// A tool without a group price is not offered: refuse before reading the body.
	if service.DeepSeekToolGroupPrice(key.Group, operation) == nil {
		fail(http.StatusForbidden, "DeepSeek "+operation+" is not offered for this group")
		return
	}
	reqLog := requestLogger(c, "handler.deepseek.tools")
	// Refuse a user who cannot pay before buffering up to 101 MiB of upload.
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, nil, service.PlatformDeepseek); err != nil {
		status, _, message, _ := billingErrorDetails(err)
		fail(status, message)
		return
	}
	limit := service.DeepSeekToolMaxBody(operation)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if handleRequestBodyReadError(c, reqLog, err) {
			return
		}
		status, message := requestBodyReadFailure(reqLog, c.Request, err)
		fail(status, message)
		return
	}
	req, err := service.PrepareDeepSeekToolRequest(operation, c.GetHeader("Content-Type"), body)
	if err != nil {
		status := http.StatusBadRequest
		var input *service.DeepSeekToolInputError
		if errors.As(err, &input) {
			status = input.Status
		}
		fail(status, err.Error())
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		fail(http.StatusUnauthorized, "user required")
		return
	}
	// Text the supplier acts on is audited like a chat message; files are not.
	if req.AuditText != "" {
		auditBody, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": req.AuditText}}})
		if decision := h.checkSecurityAudit(c, reqLog, key, subject, service.ContentModerationProtocolOpenAIChat, "deepseek-tools-"+operation, auditBody); decision != nil && !decision.AllowNextStage {
			status := decision.HTTPStatus
			if status == 0 {
				status = http.StatusForbidden
			}
			fail(status, decision.ClientMessage)
			return
		}
	}
	release, acquired := h.acquireDeepSeekToolUserSlot(c, operation, subject.UserID, subject.Concurrency, reqLog)
	if !acquired {
		return
	}
	if release != nil {
		defer release()
	}
	meta := service.OpenAIRecordUsageInput{InboundEndpoint: c.Request.URL.Path, UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c), APIKeyService: h.apiKeyService, PricingAt: started}
	if err = h.gatewayService.ServeDeepSeekTool(c.Request.Context(), c.Writer, key, req, meta); err != nil {
		status, _, message, _ := billingErrorDetails(err)
		fail(status, message)
	}
}

// acquireDeepSeekToolUserSlot takes the user's concurrency slot. OCR and ASR
// keep their supplier call running after a client disconnect, so their slot is
// held until the handler returns instead of being freed with the request
// context; otherwise disconnecting and resending would run any number of them.
func (h *OpenAIGatewayHandler) acquireDeepSeekToolUserSlot(c *gin.Context, operation string, userID int64, concurrency int, reqLog *zap.Logger) (func(), bool) {
	streamStarted := false
	if operation != service.DeepSeekToolOCR && operation != service.DeepSeekToolASR {
		return h.acquireResponsesUserSlot(c, userID, concurrency, false, &streamStarted, reqLog)
	}
	release, err := h.concurrencyHelper.AcquireUserSlotWithWait(c, userID, concurrency, false, &streamStarted)
	if err != nil {
		reqLog.Warn("deepseek_tools.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", streamStarted)
		return nil, false
	}
	return release, true
}
