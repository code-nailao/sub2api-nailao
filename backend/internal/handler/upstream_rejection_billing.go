package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Never expose repository or provider details through a failed financial admission.
func upstreamRejectionAdmissionError(err error) (int, string, string) {
	switch {
	case errors.Is(err, service.ErrUpstreamRejectionPlatformQuotaUnsupported):
		return http.StatusUnprocessableEntity, "upstream_rejection_platform_quota_unsupported", "当前用户的平台限额与此收费策略暂不兼容，请联系管理员"
	case errors.Is(err, service.ErrInsufficientBalance):
		return http.StatusPaymentRequired, "insufficient_balance", "Insufficient available balance for this request"
	case errors.Is(err, service.ErrAPIKeyQuotaExhausted):
		return http.StatusTooManyRequests, "api_key_quota_exhausted", "Insufficient API key quota for this request"
	case errors.Is(err, service.ErrAPIKeyRateLimit5hExceeded),
		errors.Is(err, service.ErrAPIKeyRateLimit1dExceeded),
		errors.Is(err, service.ErrAPIKeyRateLimit7dExceeded):
		return http.StatusTooManyRequests, "api_key_rate_limit_exceeded", "Insufficient API key window allowance for this request"
	default:
		return http.StatusServiceUnavailable, "billing_unavailable", "Request billing is temporarily unavailable"
	}
}

// The durable result owns the only settlement path, even if cyber metadata was
// also observed. Keep the cyber event, but do not create a second token bill.
func shouldRecordCyberFallbackUsage(forwardErr error, result *service.OpenAIForwardResult, reservationActive bool) bool {
	return !reservationActive && forwardErr != nil && (result == nil ||
		(result.UpstreamRejection == nil && result.UpstreamRejectionReservationID == ""))
}

func requiresUpstreamRejectionHTTP(apiKey *service.APIKey) bool {
	return apiKey != nil && apiKey.Group != nil && apiKey.Group.UpstreamRejectionBillingEnabled
}
