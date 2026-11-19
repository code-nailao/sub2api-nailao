//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// blockingRateAdmissionGate admits the concurrency slot immediately and holds
// rate admission until the supplied context ends.
type blockingRateAdmissionGate struct {
	rateCalls atomic.Int64
}

func (g *blockingRateAdmissionGate) acquire(context.Context) (func(), error) {
	return func() {}, nil
}

func (g *blockingRateAdmissionGate) acquireRate(ctx context.Context) (func(), error) {
	g.rateCalls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func withStopContexts(svc *TokenRefreshService) {
	runCtx, runCancel := context.WithCancel(context.Background())
	stopCtx, stopCancel := context.WithCancel(runCtx)
	svc.runCtx, svc.runCancel = runCtx, runCancel
	svc.stopCtx, svc.stopCancel = stopCtx, stopCancel
	svc.stopCh = make(chan struct{})
}

func TestTokenRefreshService_ParentCancelDuringAttemptStillPersistsRotatedCredentials(t *testing.T) {
	repo := &poolHealthAccountRepo{}
	ctx, cancel := context.WithCancel(context.Background())
	refresher := &poolHealthRefresher{
		cancel:         cancel,
		delay:          50 * time.Millisecond,
		newCredentials: map[string]any{"access_token": "new-token", "refresh_token": "rotated-token"},
	}
	svc := newPoolHealthService(repo, refresher, config.TokenRefreshConfig{MaxRetries: 3, AttemptTimeoutSeconds: 5})
	account := grokPoolAccount(51)

	err := svc.refreshWithRetry(ctx, &account, refresher, nil, time.Hour)

	require.ErrorIs(t, err, context.Canceled, "the stopped caller still observes its own cancellation")
	require.Equal(t, int64(1), refresher.calls.Load(), "no retry starts after the parent ended")
	_, updatedIDs, setErrorCalls, setTempUnschedCalls := repo.snapshot()
	require.Equal(t, []int64{51}, updatedIDs, "credentials returned by an in-flight attempt must be persisted")
	require.Zero(t, setErrorCalls)
	require.Zero(t, setTempUnschedCalls)
}

func TestTokenRefreshService_ParentCancelDuringRateAdmissionStartsNoUpstreamCall(t *testing.T) {
	repo := &poolHealthAccountRepo{}
	refresher := &poolHealthRefresher{newCredentials: map[string]any{"access_token": "new-token"}}
	svc := newPoolHealthService(repo, refresher, config.TokenRefreshConfig{MaxRetries: 1, AttemptTimeoutSeconds: 5})
	gate := &blockingRateAdmissionGate{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	account := grokPoolAccount(52)

	started := time.Now()
	err := svc.refreshWithRetryWithRateGate(ctx, &account, refresher, refresher, time.Hour, gate)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), 2*time.Second, "rate admission must follow the parent, not the attempt timeout")
	require.Equal(t, int64(1), gate.rateCalls.Load())
	require.Zero(t, refresher.calls.Load(), "an attempt still waiting for rate admission must not reach the provider")
	_, updatedIDs, _, _ := repo.snapshot()
	require.Empty(t, updatedIDs)
}

func TestTokenRefreshService_AlreadyEndedParentStartsNoUpstreamCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int64
	acquire := abortRateAdmissionWithParent(ctx, func(context.Context) (func(), error) {
		return func() { calls.Add(1) }, nil
	})

	release, err := acquire(context.Background())

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	require.Equal(t, int64(1), calls.Load(), "an admission that succeeded without waiting is released")
}

func TestTokenRefreshService_StopWaitsForInFlightAttemptToPersist(t *testing.T) {
	account := grokPoolAccount(53)
	repo := &poolHealthAccountRepo{pages: map[int64][]Account{0: {account}}}
	refresher := &poolHealthRefresher{
		delay:          200 * time.Millisecond,
		newCredentials: map[string]any{"access_token": "new-token", "refresh_token": "rotated-token"},
	}
	svc := newPoolHealthService(repo, refresher, config.TokenRefreshConfig{
		Enabled:               true,
		CheckIntervalMinutes:  60,
		MaxRetries:            1,
		CandidatePageSize:     10,
		ProviderConcurrency:   1,
		ProviderQPS:           10000,
		AttemptTimeoutSeconds: 5,
	})
	withStopContexts(svc)

	svc.Start()
	require.Eventually(t, func() bool { return refresher.active.Load() == 1 }, 2*time.Second, time.Millisecond)
	svc.Stop()

	require.Equal(t, int64(1), refresher.calls.Load())
	_, updatedIDs, setErrorCalls, _ := repo.snapshot()
	require.Equal(t, []int64{53}, updatedIDs, "Stop must not discard credentials from an attempt already in flight")
	require.Zero(t, setErrorCalls)
	require.ErrorIs(t, svc.runCtx.Err(), context.Canceled, "Stop finishes with the hard stop released")
}

func TestTokenRefreshService_StopReturnsWithinAttemptBound(t *testing.T) {
	account := grokPoolAccount(54)
	repo := &poolHealthAccountRepo{pages: map[int64][]Account{0: {account}}}
	refresher := &poolHealthRefresher{waitForContextDone: true}
	svc := newPoolHealthService(repo, refresher, config.TokenRefreshConfig{
		Enabled:              true,
		CheckIntervalMinutes: 60,
		MaxRetries:           1,
		CandidatePageSize:    10,
		ProviderConcurrency:  1,
		ProviderQPS:          10000,
	})
	// An attempt that only ends on cancellation is still bounded by the attempt
	// timeout, so Stop cannot hang on a provider that never answers.
	svc.attemptTimeoutOverride = 50 * time.Millisecond
	withStopContexts(svc)

	svc.Start()
	require.Eventually(t, func() bool { return refresher.active.Load() == 1 }, 2*time.Second, time.Millisecond)
	started := time.Now()
	svc.Stop()

	require.Less(t, time.Since(started), svc.attemptTimeout()+tokenRefreshStopGraceMargin+time.Second)
	_, updatedIDs, _, _ := repo.snapshot()
	require.Empty(t, updatedIDs)
}
