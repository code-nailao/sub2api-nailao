package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type groupTierSettingRepo struct{ *settingHandlerRepoStub }

func (r *groupTierSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func TestUpdateSettingsPreservesOmittedGroupTierSwitch(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		include bool
		initial bool
		want    bool
	}{
		{"older admin page", nil, false, true, true},
		{"null inherits", nil, true, true, true},
		{"explicit disable", false, true, true, false},
		{"explicit enable", true, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial, err := json.Marshal(service.OpenAIFastPolicySettings{GroupTierPoliciesEnabled: test.initial})
			require.NoError(t, err)
			repo := &groupTierSettingRepo{&settingHandlerRepoStub{values: map[string]string{
				service.SettingKeyOpenAIFastPolicySettings: string(initial),
			}}}
			handler := NewSettingHandler(service.NewSettingService(repo, &config.Config{
				Default: config.DefaultConfig{UserConcurrency: 5},
			}), nil, nil, nil, nil, nil, nil)
			policy := map[string]any{"rules": []map[string]any{{"service_tier": "ultrafast", "action": "pass", "scope": "all"}}}
			if test.include {
				policy["group_tier_policies_enabled"] = test.value
			}
			response := doUpdateSettings(t, handler, map[string]any{"openai_fast_policy_settings": policy}, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var stored service.OpenAIFastPolicySettings
			require.NoError(t, json.Unmarshal([]byte(repo.values[service.SettingKeyOpenAIFastPolicySettings]), &stored))
			require.Equal(t, test.want, stored.GroupTierPoliciesEnabled)
			require.Len(t, stored.Rules, 1)
			require.Equal(t, "ultrafast", stored.Rules[0].ServiceTier)
		})
	}
}
