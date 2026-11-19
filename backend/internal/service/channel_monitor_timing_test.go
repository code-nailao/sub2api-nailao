package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestChannelMonitorProbeTimingValidationAndFallback(t *testing.T) {
	for _, v := range [][2]int{{20, 60}, {1, 2}, {299, 300}, {25, 90}} {
		require.NoError(t, ValidateChannelMonitorProbeTiming(v[0], v[1]))
	}
	for _, v := range [][2]int{{0, 60}, {-1, 60}, {20, 20}, {21, 20}, {20, 301}, {1, 1}} {
		require.Error(t, ValidateChannelMonitorProbeTiming(v[0], v[1]))
	}
	defaults := defaultChannelMonitorProbeTiming()
	require.Equal(t, 20*time.Second, defaults.DegradedThreshold)
	require.Equal(t, 60*time.Second, defaults.RequestTimeout)
	for _, values := range []map[string]string{nil, {}, {SettingKeyChannelMonitorDegradedThresholdSeconds: "oops"}, {SettingKeyChannelMonitorRequestTimeoutSeconds: "301"}, {SettingKeyChannelMonitorDegradedThresholdSeconds: "90", SettingKeyChannelMonitorRequestTimeoutSeconds: "60"}} {
		require.Equal(t, defaults, parseChannelMonitorProbeTiming(values))
	}
	require.Equal(t, channelMonitorProbeTiming{25 * time.Second, 90 * time.Second}, parseChannelMonitorProbeTiming(map[string]string{SettingKeyChannelMonitorDegradedThresholdSeconds: "25", SettingKeyChannelMonitorRequestTimeoutSeconds: "90"}))
}

func TestChannelMonitorTimingSnapshotDoesNotChange(t *testing.T) {
	original := ChannelMonitorRuntime{DegradedThresholdSeconds: 20, RequestTimeoutSeconds: 60}
	ctx := withChannelMonitorProbeTiming(context.Background(), original.probeTiming())
	original.DegradedThresholdSeconds = 1
	original.RequestTimeoutSeconds = 120
	require.Equal(t, channelMonitorProbeTiming{20 * time.Second, 60 * time.Second}, channelMonitorProbeTimingFromContext(ctx))
	require.Equal(t, MonitorStatusOperational, finalizeOperationalOrDegraded(&CheckResult{}, 19*time.Second, 19000, channelMonitorProbeTimingFromContext(ctx).DegradedThreshold).Status)
	require.Equal(t, MonitorStatusDegraded, finalizeOperationalOrDegraded(&CheckResult{}, 20*time.Second, 20000, channelMonitorProbeTimingFromContext(ctx).DegradedThreshold).Status)
}
