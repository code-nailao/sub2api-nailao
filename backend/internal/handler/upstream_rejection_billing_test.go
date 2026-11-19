package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpstreamRejectionAdmissionError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"platform_quota", service.ErrUpstreamRejectionPlatformQuotaUnsupported, http.StatusUnprocessableEntity, "upstream_rejection_platform_quota_unsupported"},
		{"balance", service.ErrInsufficientBalance, http.StatusPaymentRequired, "insufficient_balance"},
		{"quota", service.ErrAPIKeyQuotaExhausted, http.StatusTooManyRequests, "api_key_quota_exhausted"},
		{"five_hour", service.ErrAPIKeyRateLimit5hExceeded, http.StatusTooManyRequests, "api_key_rate_limit_exceeded"},
		{"daily", service.ErrAPIKeyRateLimit1dExceeded, http.StatusTooManyRequests, "api_key_rate_limit_exceeded"},
		{"weekly", service.ErrAPIKeyRateLimit7dExceeded, http.StatusTooManyRequests, "api_key_rate_limit_exceeded"},
		{"database", errors.New("private connection details"), http.StatusServiceUnavailable, "billing_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := upstreamRejectionAdmissionError(fmt.Errorf("wrapped: %w", tc.err))
			require.Equal(t, tc.status, status)
			require.Equal(t, tc.code, code)
			require.NotContains(t, message, "private")
		})
	}
}

func TestUpstreamRejectionDoesNotCreateSecondCyberUsageBill(t *testing.T) {
	err := errors.New("upstream denied")
	require.True(t, shouldRecordCyberFallbackUsage(err, nil, false))
	require.True(t, shouldRecordCyberFallbackUsage(err, &service.OpenAIForwardResult{}, false))
	require.False(t, shouldRecordCyberFallbackUsage(nil, nil, false))
	require.False(t, shouldRecordCyberFallbackUsage(err, nil, true), "unknown reservation with no result must not create a separate cyber bill")
	require.False(t, shouldRecordCyberFallbackUsage(err, &service.OpenAIForwardResult{
		UpstreamRejection: &service.UpstreamRejectionEvidence{RuleID: "refusal"},
	}, false))
	require.False(t, shouldRecordCyberFallbackUsage(err, &service.OpenAIForwardResult{
		UpstreamRejectionReservationID: "server-owned-reservation",
	}, false))
}

func TestUpstreamRejectionUsageCannotBeDroppedOnWorkerOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *service.OpenAIForwardResult
	}{
		{"zero_token_rejection", &service.OpenAIForwardResult{UpstreamRejection: &service.UpstreamRejectionEvidence{RuleID: "refusal"}}},
		{"normal_result_with_reserved_money", &service.OpenAIForwardResult{UpstreamRejectionReservationID: "server-owned-reservation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
				WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second,
				OverflowPolicy: "drop", OverflowSamplePercent: 0, AutoScaleEnabled: false,
			})
			block, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(pool.Stop)
			t.Cleanup(func() { close(release) })
			pool.Submit(func(context.Context) { close(block); <-release })
			<-block
			pool.Submit(func(context.Context) {})
			var called atomic.Int32
			h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
			h.submitOpenAIUsageRecordTask(context.Background(), tc.result, func(context.Context) { called.Add(1) })
			require.EqualValues(t, 1, called.Load(), "money-critical request must settle synchronously on overflow")
		})
	}
}
