package service

import (
	"context"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"strconv"
	"time"
)

const monitorMaxRequestTimeout = 300 * time.Second

type channelMonitorProbeTiming struct {
	DegradedThreshold time.Duration
	RequestTimeout    time.Duration
}
type channelMonitorProbeTimingContextKey struct{}

// ValidateChannelMonitorProbeTiming rejects invalid admin writes rather than
// silently clamping them. Persisted legacy/missing values use defaults on read.
func ValidateChannelMonitorProbeTiming(degradedSeconds, timeoutSeconds int) error {
	if degradedSeconds < 1 || timeoutSeconds < 2 || timeoutSeconds > int(monitorMaxRequestTimeout/time.Second) || degradedSeconds >= timeoutSeconds {
		return infraerrors.BadRequest("CHANNEL_MONITOR_INVALID_TIMING", "yellow threshold must be at least 1 second and lower than the request timeout (2-300 seconds)")
	}
	return nil
}
func defaultChannelMonitorProbeTiming() channelMonitorProbeTiming {
	return channelMonitorProbeTiming{DegradedThreshold: monitorDegradedThreshold, RequestTimeout: monitorRequestTimeout}
}
func parseChannelMonitorProbeTiming(values map[string]string) channelMonitorProbeTiming {
	degraded, e1 := strconv.Atoi(values[SettingKeyChannelMonitorDegradedThresholdSeconds])
	timeout, e2 := strconv.Atoi(values[SettingKeyChannelMonitorRequestTimeoutSeconds])
	if e1 != nil {
		degraded = int(monitorDegradedThreshold / time.Second)
	}
	if e2 != nil {
		timeout = int(monitorRequestTimeout / time.Second)
	}
	if ValidateChannelMonitorProbeTiming(degraded, timeout) != nil {
		return defaultChannelMonitorProbeTiming()
	}
	return channelMonitorProbeTiming{DegradedThreshold: time.Duration(degraded) * time.Second, RequestTimeout: time.Duration(timeout) * time.Second}
}
func (r ChannelMonitorRuntime) probeTiming() channelMonitorProbeTiming {
	if ValidateChannelMonitorProbeTiming(r.DegradedThresholdSeconds, r.RequestTimeoutSeconds) != nil {
		return defaultChannelMonitorProbeTiming()
	}
	return channelMonitorProbeTiming{DegradedThreshold: time.Duration(r.DegradedThresholdSeconds) * time.Second, RequestTimeout: time.Duration(r.RequestTimeoutSeconds) * time.Second}
}
func withChannelMonitorProbeTiming(ctx context.Context, timing channelMonitorProbeTiming) context.Context {
	return context.WithValue(ctx, channelMonitorProbeTimingContextKey{}, timing)
}
func channelMonitorProbeTimingFromContext(ctx context.Context) channelMonitorProbeTiming {
	if timing, ok := ctx.Value(channelMonitorProbeTimingContextKey{}).(channelMonitorProbeTiming); ok {
		return timing
	}
	return defaultChannelMonitorProbeTiming()
}
