//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type sourcesRecoveryBlockedRepo struct {
	GeminiCachedContentRepository
	entered chan struct{}
	release chan struct{}
}

func (r *sourcesRecoveryBlockedRepo) ListStaleMutations(context.Context, int) ([]int64, error) {
	close(r.entered)
	<-r.release
	return nil, nil
}

func TestSourcesCachedContentStopWaitsForNativeRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &sourcesRecoveryBlockedRepo{entered: make(chan struct{}), release: make(chan struct{})}
		svc := NewGeminiCachedContentService(repo, nil, nil)
		svc.Start()
		<-repo.entered // The real native ticker runs in the synctest clock.
		stopped := make(chan struct{})
		go func() { svc.Stop(); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while a native database recovery was in flight")
		default:
		}
		close(repo.release)
		<-stopped
		svc.Start() // A stopped generation cannot silently reacquire ownership.
		svc.Stop()
	})
}

func TestSourcesExternalSettingsRefreshPreservesNativeCacheBehavior(t *testing.T) {
	resetBackendModeTestCache(t)
	repo := &bmUpdateRepoStub{getValueFn: func(context.Context, string) (string, error) {
		t.Fatal("freshly refreshed native policy cache should not fetch stale settings")
		return "", nil
	}}
	svc := NewSettingService(repo, &config.Config{})
	invalidations := 0
	svc.SetOnUpdateCallback(func() { invalidations++ })
	svc.refreshSourcesSettings(context.Background(), map[string]string{SettingKeyBackendModeEnabled: "true"})
	require.True(t, svc.IsBackendModeEnabled(context.Background()))
	svc.refreshSourcesSettings(context.Background(), map[string]string{SettingKeyBackendModeEnabled: "false"})
	require.False(t, svc.IsBackendModeEnabled(context.Background()))
	require.Equal(t, 2, invalidations, "HTML and frame-origin caches must observe owner-side settings changes")
	require.Empty(t, repo.updates, "refresh must not write settings back to the shared database")
}
