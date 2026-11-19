package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"sources.local/mediaengine"
)

func ProvideSourcesAsyncImageHandler(tasks *service.ImageTaskService, gateway *OpenAIGatewayHandler, engine *service.SourcesMediaEngine) *AsyncImageHandler {
	h := NewAsyncImageHandler(tasks, gateway)
	h.mediaEngine = engine
	return h
}
func sourcesImageOwner(c *gin.Context) (*service.APIKey, mediaengine.Owner, bool) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.GroupID == nil || key.Group == nil {
		return nil, mediaengine.Owner{}, false
	}
	return key, mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}, true
}
func (h *AsyncImageHandler) trySourcesMediaEngineSubmit(c *gin.Context) bool {
	if h == nil || h.mediaEngine == nil {
		return false
	}
	key, _, ok := sourcesImageOwner(c)
	if !ok {
		return false
	}
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	handles, err := h.mediaEngine.Handles(key, idem)
	if err != nil {
		imageTaskJSONError(c, 503, "api_error", "image task registry unavailable")
		return true
	}
	if !handles {
		return false
	}
	if len(idem) > 200 {
		imageTaskJSONError(c, 400, "invalid_request_error", "idempotency key is too long")
		return true
	}
	if idem == "" {
		idem = uuid.NewString()
	}
	c.Header("Idempotency-Key", idem)
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		imageTaskJSONError(c, 400, "invalid_request_error", "invalid image request body")
		return true
	}
	admission, err := h.mediaEngine.BeginAdmission(c.Request.Context())
	if err != nil {
		sourcesImageError(c, err)
		return true
	}
	known := true
	defer func() { admission.Finish(known) }()
	if admission.Mode == processrole.AdmissionLegacy {
		// The authenticated legacy handler owns audit, concurrency and billing.
		// Taking slots here too would deadlock users with concurrency one.
		response, confirmed, err := h.mediaEngine.ForwardLegacyAdmission(c.Request.Context(), key, c.Request.URL.Path, c.Request.Header, body, idem, c.ClientIP())
		known = confirmed
		if err != nil {
			sourcesImageError(c, err)
			return true
		}
		for name, value := range response.Headers {
			c.Header(name, value)
		}
		c.Data(response.Status, "application/json", response.Body)
		return true
	}
	if !h.auditSourcesMediaEngine(c, key, body) {
		return true
	}
	if h.openAI == nil {
		imageTaskJSONError(c, 503, "api_error", "image gateway unavailable")
		return true
	}
	imageRelease, acquired := h.openAI.acquireImageGenerationSlot(c, false)
	if !acquired {
		return true
	}
	if imageRelease != nil {
		defer imageRelease()
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		sourcesImageError(c, mediaengine.ErrDenied)
		return true
	}
	streamStarted := false
	release, acquired := h.openAI.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, requestLogger(c, "handler.sources_media_engine"))
	if !acquired {
		return true
	}
	if release != nil {
		defer release()
	}
	task, err := h.mediaEngine.Submit(c.Request.Context(), key, c.Request.URL.Path, c.GetHeader("Content-Type"), body, idem, c.ClientIP(), c.GetHeader("User-Agent"), admission)
	if err != nil {
		sourcesImageError(c, err)
		return true
	}
	c.Header("Idempotency-Key", idem)
	c.Header("Location", imageTaskPollURL(c.Request.URL.Path, task.ID))
	c.Header("Retry-After", "3")
	c.Header("Cache-Control", "no-store")
	value := sourcesPublicImageTask(task)
	if !h.addSourcesResult(c, task, value) {
		return true
	}
	value["poll_url"] = imageTaskPollURL(c.Request.URL.Path, task.ID)
	c.JSON(http.StatusAccepted, value)
	return true
}
func sourcesImageError(c *gin.Context, err error) {
	var billing *service.SourcesImageBillingError
	var validation *service.SourcesImageRequestError
	if errors.As(err, &billing) {
		status, code, message, retry := billingErrorDetails(billing.Cause)
		if retry > 0 {
			c.Header("Retry-After", strconv.Itoa(retry))
		}
		imageTaskJSONError(c, status, code, message)
		return
	}

	switch {
	case errors.Is(err, mediaengine.ErrNotFound):
		imageTaskJSONError(c, 404, "not_found_error", "image task not found")
	case errors.Is(err, mediaengine.ErrConflict):
		imageTaskJSONError(c, 409, "idempotency_conflict", "image request conflicts with the original task")
	case errors.Is(err, mediaengine.ErrDenied):
		imageTaskJSONError(c, 403, "permission_error", "image task admission denied")
	case errors.Is(err, mediaengine.ErrCapacity):
		imageTaskJSONError(c, 429, "rate_limit_error", "image task record capacity reached")
	case errors.As(err, &validation):
		imageTaskJSONError(c, 400, "invalid_request_error", validation.Error())
	default:
		imageTaskJSONError(c, 503, "api_error", "image generation is temporarily unavailable; please try again later")
	}
}
func (h *AsyncImageHandler) getSourcesMediaEngine(c *gin.Context) {
	if h.mediaEngine == nil {
		sourcesImageError(c, mediaengine.ErrNotFound)
		return
	}
	_, owner, ok := sourcesImageOwner(c)
	if !ok {
		sourcesImageError(c, mediaengine.ErrDenied)
		return
	}
	task, err := h.mediaEngine.GetTask(owner, c.Param("task_id"))
	if err != nil {
		sourcesImageError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if task.Phase == "archived" {
		imageTaskJSONError(c, 410, "task_expired", "image result retention period ended")
		return
	}
	value := sourcesPublicImageTask(task)
	if !h.addSourcesResult(c, task, value) {
		return
	}
	c.JSON(200, value)
}
func sourcesPublicImageTask(t *mediaengine.Task) gin.H {
	status := "processing"
	switch t.Phase {
	case "completed":
		status = "completed"
	case "failed":
		status = "failed"
	}
	out := gin.H{"id": t.ID, "task_id": t.ID, "object": "image.generation.task", "status": status, "created_at": t.CreatedAt, "expires_at": t.CreatedAt + int64(7*24*time.Hour/time.Second)}
	if t.Phase == "completed" {
		out["http_status"] = 200
	}
	if t.Phase == "failed" {
		out["error"] = gin.H{"code": sourcesPublicImageFailure(t.Failure), "message": "image task ended without a successful result"}
	}
	if t.Phase == "unknown" || t.Phase == "delivery_review" {
		out["requires_review"] = true
		out["review_reason"] = "image_task_requires_review"
	}
	return out
}

// Task failures are internal state-machine details, never raw public error codes.
func sourcesPublicImageFailure(reason string) string {
	switch reason {
	case "admission_denied":
		return "image_request_denied"
	case "pre_submit_expired":
		return "image_task_expired"
	case "operator_released_unknown", "automatic_unknown_release":
		return "image_task_closed"
	default:
		return "image_generation_failed"
	}
}

// Admin routes use native admin authentication and audit middleware. Never return
// the encrypted billing ticket, provider credential, request body or source URL.
func (h *AsyncImageHandler) SourcesMediaEngineReview(c *gin.Context) {
	if h.mediaEngine == nil {
		sourcesImageError(c, mediaengine.ErrNotFound)
		return
	}
	phase := c.DefaultQuery("phase", "unknown")
	tasks, err := h.mediaEngine.ReviewPage(phase, c.Query("after"))
	if err != nil {
		sourcesImageError(c, err)
		return
	}
	items := []gin.H{}
	for _, t := range tasks {
		item := sourcesPublicImageTask(t)
		item["user_id"] = t.Owner.UserID
		item["api_key_id"] = t.Owner.KeyID
		item["group_id"] = t.Owner.GroupID
		item["account_id"] = t.AccountID
		item["phase"] = t.Phase
		if model, amount, e := service.SourcesImageReviewSummary(t); e == nil {
			item["model"] = model
			item["reserved_amount"] = amount
		}
		item["upstream_reference"] = t.UpstreamID
		recovery := h.mediaEngine.RecoverySummary(t)
		item["recovery_status"] = recovery.Status
		item["manual_action_required"] = recovery.ManualActionRequired
		if recovery.DeadlineAt > 0 {
			item["recovery_deadline_at"] = recovery.DeadlineAt
		}
		items = append(items, item)
	}
	c.JSON(200, gin.H{"code": 0, "data": gin.H{"items": items}})
}
func (h *AsyncImageHandler) SourcesMediaEngineResolve(c *gin.Context) {
	if h.mediaEngine == nil {
		sourcesImageError(c, mediaengine.ErrNotFound)
		return
	}
	if h.mediaEngine.Engine == nil {
		sourcesImageError(c, mediaengine.ErrDenied)
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		sourcesImageError(c, mediaengine.ErrDenied)
		return
	}
	var in struct {
		Action    string `json:"action"`
		Reference string `json:"reference"`
		Reason    string `json:"reason"`
		Confirm   bool   `json:"confirm"`
	}
	if c.ShouldBindJSON(&in) != nil || !in.Confirm {
		imageTaskJSONError(c, 400, "invalid_request_error", "explicit administrator confirmation required")
		return
	}
	if err := h.mediaEngine.Engine.Resolve(c.Request.Context(), c.Param("task_id"), in.Action, in.Reference, subject.UserID, in.Reason); err != nil {
		sourcesImageError(c, err)
		return
	}
	c.JSON(200, gin.H{"code": 0, "data": gin.H{"operator_id": strconv.FormatInt(subject.UserID, 10), "status": "reconciliation_queued"}})
}

func (h *AsyncImageHandler) lookupSourcesMediaEngine(c *gin.Context) {
	if h.mediaEngine == nil {
		sourcesImageError(c, mediaengine.ErrNotFound)
		return
	}
	_, owner, ok := sourcesImageOwner(c)
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !ok || idem == "" || len(idem) > 200 {
		sourcesImageError(c, mediaengine.ErrDenied)
		return
	}
	task, err := h.mediaEngine.Lookup(owner, idem)
	if err != nil {
		sourcesImageError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if task.Phase == "archived" {
		imageTaskJSONError(c, 410, "task_expired", "image result retention period ended")
		return
	}
	out := sourcesPublicImageTask(task)
	if !h.addSourcesResult(c, task, out) {
		return
	}
	out["poll_url"] = imageTaskPollURL(c.Request.URL.Path, task.ID)
	c.JSON(200, out)
}
func (h *AsyncImageHandler) auditSourcesMediaEngine(c *gin.Context, key *service.APIKey, body []byte) bool {
	model, input, err := service.SourcesMediaEngineAuditInput(c.Request.URL.Path, c.GetHeader("Content-Type"), body)
	if err != nil {
		imageTaskJSONError(c, 400, "invalid_request_error", err.Error())
		return false
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || h.openAI == nil {
		sourcesImageError(c, mediaengine.ErrDenied)
		return false
	}
	log := requestLogger(c, "handler.sources_media_engine.security_audit", zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", key.ID), zap.String("model", model))
	decision := h.openAI.checkSecurityAudit(c, log, key, subject, service.ContentModerationProtocolOpenAIImages, model, input)
	if decision != nil && !decision.AllowNextStage {
		h.openAI.openAISecurityAuditError(c, decision)
		return false
	}
	return true
}

func (h *AsyncImageHandler) addSourcesResult(c *gin.Context, t *mediaengine.Task, out gin.H) bool {
	if t.Phase != "completed" {
		return true
	}
	result, err := h.mediaEngine.Result(c.Request.Context(), t)
	if err != nil {
		sourcesImageError(c, err)
		return false
	}
	out["result"] = result
	return true
}
