package admin

import (
	"bytes"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingHandler_ChannelMonitorTimingPatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, body      string
		status          int
		yellow, timeout string
	}{
		{"set both", `{"channel_monitor_degraded_threshold_seconds":20,"channel_monitor_request_timeout_seconds":60}`, 200, "20", "60"},
		{"partial", `{"channel_monitor_degraded_threshold_seconds":25}`, 200, "25", "90"},
		{"omitted", `{"site_name":"timing-test"}`, 200, "30", "90"},
		{"zero", `{"channel_monitor_degraded_threshold_seconds":0}`, 400, "30", "90"},
		{"equal", `{"channel_monitor_request_timeout_seconds":30}`, 400, "30", "90"},
		{"too long", `{"channel_monitor_request_timeout_seconds":301}`, 400, "30", "90"},
		{"fractional", `{"channel_monitor_degraded_threshold_seconds":1.5}`, 400, "30", "90"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &settingHandlerRepoStub{values: map[string]string{service.SettingKeyChannelMonitorDegradedThresholdSeconds: "30", service.SettingKeyChannelMonitorRequestTimeoutSeconds: "90", service.SettingKeyChannelMonitorMode: "v1"}}
			handler := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", bytes.NewBufferString(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.UpdateSettings(c)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.yellow, repo.values[service.SettingKeyChannelMonitorDegradedThresholdSeconds])
			require.Equal(t, tt.timeout, repo.values[service.SettingKeyChannelMonitorRequestTimeoutSeconds])
			if tt.name == "omitted" {
				require.NotContains(t, repo.lastUpdates, service.SettingKeyChannelMonitorDegradedThresholdSeconds)
				require.NotContains(t, repo.lastUpdates, service.SettingKeyChannelMonitorRequestTimeoutSeconds)
			}
			if tt.status == 400 {
				require.Nil(t, repo.lastUpdates)
			}
			if tt.status == 200 {
				require.Contains(t, rec.Body.String(), `"channel_monitor_request_timeout_seconds":`)
			}
		})
	}
}
