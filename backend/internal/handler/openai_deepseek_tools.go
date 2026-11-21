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

const deepSeekToolUploadAdmitted = "deepseek_tool_upload_admitted"

// AdmitDeepSeekToolBody keeps the standalone tool's access check and size cap
// ahead of the gateway middleware that prereads request bodies.
func (h *OpenAIGatewayHandler) AdmitDeepSeekToolBody(c *gin.Context) {
	operation, ok := strings.CutPrefix(c.FullPath(), "/v1/tools/")
	if !ok || !service.IsDeepSeekToolOperation(operation) {
		return
	}
	key, _ := middleware2.GetAPIKeyFromContext(c)
	// Composite routes resolve a model group after ingress validation. Check
	// access in the handler if that routing changes the key's group.
	if key == nil || key.Group == nil || key.Group.Platform != service.PlatformComposite {
		if !h.checkDeepSeekToolAccess(c, key, operation) {
			c.Abort()
			return
		}
		c.Set(deepSeekToolUploadAdmitted, true)
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.DeepSeekToolMaxBody(operation))
}

func writeDeepSeekToolError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": "deepseek_tool_error", "message": message}})
}

func (h *OpenAIGatewayHandler) checkDeepSeekToolAccess(c *gin.Context, key *service.APIKey, operation string) bool {
	if key == nil {
		writeDeepSeekToolError(c, http.StatusUnauthorized, "API key required")
		return false
	}
	if key.Group == nil || key.Group.Platform != service.PlatformDeepseek || !key.Group.DeepSeekTools.Enabled {
		writeDeepSeekToolError(c, http.StatusForbidden, "DeepSeek tools are disabled for this group")
		return false
	}
	if !service.IsDeepSeekToolOperation(operation) {
		writeDeepSeekToolError(c, http.StatusNotFound, "unsupported DeepSeek tool")
		return false
	}
	if service.DeepSeekToolGroupPrice(key.Group, operation) == nil {
		writeDeepSeekToolError(c, http.StatusForbidden, "DeepSeek "+operation+" is not offered for this group")
		return false
	}
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, nil, service.PlatformDeepseek); err != nil {
		status, _, message, _ := billingErrorDetails(err)
		writeDeepSeekToolError(c, status, message)
		return false
	}
	return true
}

// DeepSeekTool serves the standalone tools as thin relays. It shares provider
// execution and billing with hosted tools, with no token or model fallback pricing.
func (h *OpenAIGatewayHandler) DeepSeekTool(c *gin.Context) {
	started := time.Now()
	operation := strings.TrimPrefix(c.Request.URL.Path, "/v1/tools/")
	key, _ := middleware2.GetAPIKeyFromContext(c)
	if _, admitted := c.Get(deepSeekToolUploadAdmitted); !admitted && !h.checkDeepSeekToolAccess(c, key, operation) {
		return
	}
	reqLog := requestLogger(c, "handler.deepseek.tools")
	// Keep the preread buffer intact. The route capped the raw body before
	// reading it, and PrepareDeepSeekToolRequest also bounds the decoded bytes.
	if _, preread := c.Request.Body.(*pkghttputil.PrereadBody); !preread {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.DeepSeekToolMaxBody(operation))
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if handleRequestBodyReadError(c, reqLog, err) {
			return
		}
		status, message := requestBodyReadFailure(reqLog, c.Request, err)
		writeDeepSeekToolError(c, status, message)
		return
	}
	req, err := service.PrepareDeepSeekToolRequest(operation, c.GetHeader("Content-Type"), body)
	if err != nil {
		status := http.StatusBadRequest
		var input *service.DeepSeekToolInputError
		if errors.As(err, &input) {
			status = input.Status
		}
		writeDeepSeekToolError(c, status, err.Error())
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		writeDeepSeekToolError(c, http.StatusUnauthorized, "user required")
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
			writeDeepSeekToolError(c, status, decision.ClientMessage)
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
		writeDeepSeekToolError(c, status, message)
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
