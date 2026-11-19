package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeRequestSpeedGroupPublicDTO(t *testing.T) {
	g := &service.Group{ID: 2, Name: "eligible", Platform: "openai", RequestSpeedSelectionEnabled: true,
		DefaultRequestSpeedTier: "ultrafast", AvailableRequestSpeedTiers: []string{"fast", "ultrafast"},
		ForceOpenAIUltrafast: true, OpenAIServiceTierPolicy: "force_ultrafast",
		AccountGroups: []service.AccountGroup{{AccountID: 90, GroupID: 2, RequestSpeedTiers: []string{"fast"}}}}
	raw, err := json.Marshal(GroupFromService(g))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"request_speed_selection_enabled":true`)
	require.Contains(t, string(raw), `"available_request_speed_tiers":["fast","ultrafast"]`)
	require.NotContains(t, string(raw), "account_id")
	require.NotContains(t, string(raw), "force_openai_ultrafast")
	require.NotContains(t, string(raw), "openai_service_tier_policy")
}
