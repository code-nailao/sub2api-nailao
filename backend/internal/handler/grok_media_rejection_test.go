package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGrokMediaRejectionIsBillableWithoutMediaUnits(t *testing.T) {
	r := &service.OpenAIForwardResult{UpstreamRejection: &service.UpstreamRejectionEvidence{HTTPStatus: 400}}
	for _, endpoint := range []service.GrokMediaEndpoint{service.GrokMediaEndpointImagesGenerations, service.GrokMediaEndpointImagesEdits, service.GrokMediaEndpointVideosGenerations, service.GrokMediaEndpointVideosEdits} {
		require.True(t, shouldRecordGrokMediaUsage(endpoint, "grok-model", r))
	}
	require.False(t, shouldRecordGrokMediaUsage(service.GrokMediaEndpointVideoStatus, "grok-model", r))
	require.False(t, shouldRecordGrokMediaUsage(service.GrokMediaEndpointVideoContent, "grok-model", r))
}

func TestGrokVideoRejectionRequiresCreateTimeTaskBinding(t *testing.T) {
	result := &service.OpenAIForwardResult{UpstreamRejection: &service.UpstreamRejectionEvidence{HTTPStatus: 403}}
	r := &service.UpstreamRejectionReservation{
		UpstreamRejectionReserveCommand: service.UpstreamRejectionReserveCommand{RequestID: service.NewUpstreamRejectionRequestID(), Model: "grok-imagine-video-1.5"},
		Task:                            &service.UpstreamRejectionTaskBinding{TaskID: "task-owned", UpstreamModel: "grok-imagine-video-1.5"},
	}
	require.Nil(t, prepareGrokVideoRejectionBilling(result, nil, "task-owned"))
	require.Nil(t, prepareGrokVideoRejectionBilling(result, r, "task-other"))
	require.Nil(t, prepareGrokVideoRejectionBilling(&service.OpenAIForwardResult{}, r, "task-owned"))
	first := prepareGrokVideoRejectionBilling(result, r, "task-owned")
	second := prepareGrokVideoRejectionBilling(result, r, "task-owned")
	require.NotNil(t, first)
	require.Equal(t, r.RequestID, first.RequestID)
	require.Equal(t, first.RequestID, second.RequestID)
	require.Equal(t, r.RequestID, first.UpstreamRejectionReservationID)
	require.Equal(t, r.Model, first.Model)
	require.Zero(t, first.ImageCount)
	require.Zero(t, first.VideoCount)
	require.Empty(t, result.UpstreamRejectionReservationID, "caller result must not be mutated")
}

func TestGrokMediaAccountRestoresAdmittedProfileWithoutMutatingScheduler(t *testing.T) {
	a := &service.Account{ID: 7, Extra: map[string]any{"upstream_rejection_profile": "", "preserved": true}}
	r := &service.UpstreamRejectionReservation{UpstreamRejectionReserveCommand: service.UpstreamRejectionReserveCommand{
		AccountID: 7, ProviderProfile: "grok_content_rejection_v1", UpstreamCost: .1, UpstreamCurrency: "USD",
	}}
	copy := grokMediaAccountForReservation(a, r)
	require.NotSame(t, a, copy)
	require.Equal(t, "grok_content_rejection_v1", copy.Extra["upstream_rejection_profile"])
	require.Equal(t, true, copy.Extra["preserved"])
	require.Equal(t, "", a.Extra["upstream_rejection_profile"])
	r.AccountID = 8
	require.Same(t, a, grokMediaAccountForReservation(a, r))
}
