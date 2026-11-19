//go:build unit

package service

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelMonitorTimingReloadsAcrossSettingServiceInstances(t *testing.T) {
	repo := &settingPublicRepoStub{values: map[string]string{SettingKeyChannelMonitorMode: "v1"}}
	serving := NewSettingService(repo, &config.Config{})
	worker := NewSettingService(repo, &config.Config{})
	first := serving.GetChannelMonitorRuntime(context.Background())
	require.Equal(t, 20, first.DegradedThresholdSeconds)
	require.Equal(t, 60, first.RequestTimeoutSeconds)
	repo.values[SettingKeyChannelMonitorDegradedThresholdSeconds] = "35"
	repo.values[SettingKeyChannelMonitorRequestTimeoutSeconds] = "120"
	next := worker.GetChannelMonitorRuntime(context.Background())
	require.Equal(t, 35, next.DegradedThresholdSeconds)
	require.Equal(t, 120, next.RequestTimeoutSeconds)
	require.Equal(t, 20, first.DegradedThresholdSeconds)
	repo.err = errors.New("test read error")
	require.Equal(t, defaultChannelMonitorProbeTiming(), worker.GetChannelMonitorRuntime(context.Background()).probeTiming())
}

func TestChannelMonitorRunCheckHotUpdateKeepsInFlightAndHistory(t *testing.T) {
	swapMonitorHTTPClient(t)
	origPing := monitorPingHTTPClient
	monitorPingHTTPClient = &http.Client{Timeout: time.Second}
	t.Cleanup(func() { monitorPingHTTPClient = origPing })
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		started <- struct{}{}
		delay := 1100 * time.Millisecond
		if calls.Add(1) == 1 {
			delay = 2200 * time.Millisecond
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()
	settingsRepo := &settingPublicRepoStub{values: map[string]string{SettingKeyChannelMonitorMode: "v1", SettingKeyChannelMonitorDegradedThresholdSeconds: "3", SettingKeyChannelMonitorRequestTimeoutSeconds: "5"}}
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Name: "timing", Provider: MonitorProviderAnthropic, Endpoint: srv.URL, APIKey: "NEW:test-key", PrimaryModel: "test-model", CheckMode: MonitorCheckModeProbe, BodyOverrideMode: MonitorBodyOverrideModeReplace, BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}}}
	svc := newQuotaModeService(repo)
	svc.SetRuntimeReader(NewSettingService(settingsRepo, &config.Config{}))
	type result struct {
		rows []*CheckResult
		err  error
	}
	done := make(chan result, 1)
	go func() { rows, err := svc.RunCheck(context.Background(), 1); done <- result{rows, err} }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	settingsRepo.values[SettingKeyChannelMonitorDegradedThresholdSeconds] = "1"
	settingsRepo.values[SettingKeyChannelMonitorRequestTimeoutSeconds] = "2"
	first := <-done
	require.NoError(t, first.err)
	require.Equal(t, MonitorStatusOperational, first.rows[0].Status)
	second, err := svc.RunCheck(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusDegraded, second[0].Status)
	require.Len(t, repo.history, 2)
	require.Equal(t, MonitorStatusOperational, repo.history[0].Status)
	require.Equal(t, MonitorStatusDegraded, repo.history[1].Status)
	// Per-probe client copies must not mutate the test's shared client.
	require.Equal(t, 5*time.Second, monitorHTTPClient.Timeout)
}

func TestChannelMonitorDynamicTimeoutCoversHeadersAndBody(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[flushHeaders], func(t *testing.T) {
			swapMonitorHTTPClient(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if flushHeaders {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			}))
			defer srv.Close()
			ctx := withChannelMonitorProbeTiming(context.Background(), channelMonitorProbeTiming{DegradedThreshold: 10 * time.Millisecond, RequestTimeout: 80 * time.Millisecond})
			result := runCheckForModel(ctx, MonitorProviderAnthropic, srv.URL, "key", "model", nil)
			require.Equal(t, MonitorStatusError, result.Status)
			require.Contains(t, result.Message, "context deadline exceeded")
			require.Equal(t, 5*time.Second, monitorHTTPClient.Timeout)
		})
	}
}

func TestChannelMonitorDynamicTimeoutCanExceedSharedClientTimeout(t *testing.T) {
	swapMonitorHTTPClient(t)
	monitorHTTPClient.Timeout = 10 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()
	ctx := withChannelMonitorProbeTiming(context.Background(), channelMonitorProbeTiming{DegradedThreshold: 50 * time.Millisecond, RequestTimeout: 500 * time.Millisecond})
	result := runCheckForModel(ctx, MonitorProviderAnthropic, srv.URL, "key", "model", &CheckOptions{BodyOverrideMode: MonitorBodyOverrideModeReplace, BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}})
	require.Equal(t, MonitorStatusDegraded, result.Status)
	require.Equal(t, 10*time.Millisecond, monitorHTTPClient.Timeout)
}
