//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stopAwarePlanRepo struct {
	ScheduledTestPlanRepository
	listCalls atomic.Int64
	listing   chan struct{}
}

func (r *stopAwarePlanRepo) ListDue(ctx context.Context, _ time.Time) ([]*ScheduledTestPlan, error) {
	r.listCalls.Add(1)
	if r.listing != nil {
		close(r.listing)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestScheduledTestRunner_StopEndsPendingDelayWithoutListingPlans(t *testing.T) {
	repo := &stopAwarePlanRepo{}
	s := NewScheduledTestRunnerService(repo, nil, nil, nil, nil)
	s.runDelay = time.Hour
	done := make(chan struct{})
	go func() {
		s.runScheduled()
		close(done)
	}()

	s.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a run still waiting for its start delay must end when Stop begins")
	}
	require.Zero(t, repo.listCalls.Load())
}

func TestScheduledTestRunner_StopCancelsRunInFlight(t *testing.T) {
	repo := &stopAwarePlanRepo{listing: make(chan struct{})}
	s := NewScheduledTestRunnerService(repo, nil, nil, nil, nil)
	s.runDelay = time.Millisecond
	done := make(chan struct{})
	go func() {
		s.runScheduled()
		close(done)
	}()
	select {
	case <-repo.listing:
	case <-time.After(2 * time.Second):
		t.Fatal("run never started")
	}

	started := time.Now()
	s.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an in-flight run must observe the stop instead of its 5-minute budget")
	}
	require.Less(t, time.Since(started), 2*time.Second)
}
