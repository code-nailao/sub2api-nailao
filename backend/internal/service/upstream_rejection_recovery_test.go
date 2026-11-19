//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/stretchr/testify/require"
)

type rejectionRecoverySourceStub struct {
	page func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error)
}

type rejectionProjectionSourceStub struct {
	page func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error)
}

func (s *rejectionProjectionSourceStub) ListUnprojectedUpstreamRejectionsPage(ctx context.Context, q *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	return s.page(ctx, q)
}

func TestUpstreamRejectionRecoveryRepairsTextAndVideoProjectionsAfterRestart(t *testing.T) {
	// The outbox survives a process restart; only a successful usage projection
	// removes an entry. The worker has no funds mutation dependency in this path.
	pending := []UpstreamRejectionReservation{
		{UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{RequestID: "text"}, State: UpstreamRejectionRejected},
		{UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{RequestID: "video"}, State: UpstreamRejectionRejected, Task: &UpstreamRejectionTaskBinding{TaskID: "task"}},
	}
	var attempts, projected []string
	failText := true
	source := &rejectionProjectionSourceStub{page: func(ctx context.Context, q *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
		require.False(t, q.TasksOnly, "text settlements need projection recovery too")
		return &UpstreamRejectionPendingPage{Items: append([]UpstreamRejectionReservation{}, pending...)}, nil
	}}
	project := func(ctx context.Context, r *UpstreamRejectionReservation) error {
		attempts = append(attempts, r.RequestID)
		if r.RequestID == "text" && failText {
			return errors.New("usage database temporarily unavailable")
		}
		projected = append(projected, r.RequestID)
		for i := range pending {
			if pending[i].RequestID == r.RequestID {
				pending = append(pending[:i], pending[i+1:]...)
				break
			}
		}
		return nil
	}
	first := &UpstreamRejectionRecovery{projectionRepo: source, project: project}
	first.recoverUsageProjections(context.Background())
	require.Equal(t, []string{"video"}, projected)
	failText = false
	restarted := &UpstreamRejectionRecovery{projectionRepo: source, project: project}
	restarted.recoverUsageProjections(context.Background())
	restarted.recoverUsageProjections(context.Background())
	require.Equal(t, []string{"text", "video", "text"}, attempts)
	require.Equal(t, []string{"video", "text"}, projected)
	require.Empty(t, pending)
}

func TestUpstreamRejectionRecoveryReleasesOnlyStaleUnsentReservations(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	row := func(id, state string, created time.Time) UpstreamRejectionReservation {
		return UpstreamRejectionReservation{UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{RequestID: id}, State: state, CreatedAt: created}
	}
	rows := []UpstreamRejectionReservation{
		row("old_reserved", UpstreamRejectionReserved, old),
		row("fresh_reserved", UpstreamRejectionReserved, now),
		row("old_sent", UpstreamRejectionSent, old),
		row("old_unknown", UpstreamRejectionUnknown, old),
		row("racing_sent", UpstreamRejectionReserved, old),
	}
	var released []string
	w := &UpstreamRejectionRecovery{
		repo: &rejectionRecoverySourceStub{page: func(ctx context.Context, q *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
			require.False(t, q.TasksOnly)
			return &UpstreamRejectionPendingPage{Items: rows}, nil
		}},
		releaseUnsent: func(ctx context.Context, r *UpstreamRejectionReservation) error {
			if r.RequestID == "racing_sent" {
				return ErrUpstreamRejectionState
			}
			released = append(released, r.RequestID)
			return nil
		},
	}
	w.recoverUnsentReservations(context.Background())
	require.Equal(t, []string{"old_reserved"}, released)
}

func (s *rejectionRecoverySourceStub) ListPendingUpstreamRejectionsPage(ctx context.Context, q *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	return s.page(ctx, q)
}

func TestUpstreamRejectionRecoveryRoleAndStop(t *testing.T) {
	sourcesServiceProcessRoles(t, func(t *testing.T, role processrole.Role) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int64
			w := &UpstreamRejectionRecovery{
				repo: &rejectionRecoverySourceStub{page: func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
					calls.Add(1)
					return &UpstreamRejectionPendingPage{}, nil
				}},
				resolveKey: func(context.Context, *UpstreamRejectionReservation) (*APIKey, error) { return nil, nil },
				reconcile:  func(context.Context, *UpstreamRejectionReservation, *APIKey) (bool, error) { return false, nil },
			}
			processrole.StartBackground("UpstreamRejectionRecovery", w.Start)
			w.Start() // A repeated call must neither bypass serving nor duplicate scans.
			synctest.Wait()
			want := int64(1)
			if role == processrole.Serving {
				want = 0
			}
			require.Equal(t, want, calls.Load())
			time.Sleep(rejectionRecoveryInterval)
			synctest.Wait()
			require.Equal(t, want*2, calls.Load())
			w.Stop()
			w.Start() // A retired generation cannot restart.
			time.Sleep(rejectionRecoveryInterval)
			synctest.Wait()
			require.Equal(t, want*2, calls.Load())
		})
	})
}

func TestUpstreamRejectionRecoveryTaskPagingAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cursor := &UpstreamRejectionPendingCursor{RequestID: "cursor", UpdatedAt: time.Now()}
		calls := 0
		w := &UpstreamRejectionRecovery{repo: &rejectionRecoverySourceStub{page: func(ctx context.Context, q *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
			require.True(t, q.TasksOnly)
			require.Equal(t, rejectionRecoveryPageSize, q.Limit)
			calls++
			if calls == 2 {
				require.Equal(t, cursor, q.After)
				return &UpstreamRejectionPendingPage{}, nil
			}
			return &UpstreamRejectionPendingPage{Next: cursor, Items: []UpstreamRejectionReservation{
				{State: UpstreamRejectionUnknown}, // No task must never produce an upstream call.
				{State: UpstreamRejectionReserved, Task: &UpstreamRejectionTaskBinding{TaskID: "never-sent"}},
				{State: UpstreamRejectionSent, Task: &UpstreamRejectionTaskBinding{TaskID: "unresolvable"}},
				{State: UpstreamRejectionUnknown, Task: &UpstreamRejectionTaskBinding{TaskID: "pending"}},
			}}, nil
		}}}
		var resolved, polled []string
		w.resolveKey = func(ctx context.Context, r *UpstreamRejectionReservation) (*APIKey, error) {
			resolved = append(resolved, r.Task.TaskID)
			if r.Task.TaskID == "unresolvable" {
				return nil, errors.New("missing identity")
			}
			return &APIKey{}, nil
		}
		w.reconcile = func(ctx context.Context, r *UpstreamRejectionReservation, key *APIKey) (bool, error) {
			polled = append(polled, r.Task.TaskID)
			return false, nil
		}
		w.runOnce(context.Background())
		w.runOnce(context.Background())
		require.Equal(t, []string{"unresolvable", "pending"}, resolved)
		require.Equal(t, []string{"pending"}, polled)
		require.Nil(t, w.cursor)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w.runOnce(ctx)
		require.Equal(t, 2, calls)
	})
}

func TestUpstreamRejectionRecoveryStopJoinsActiveObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		w := &UpstreamRejectionRecovery{
			repo: &rejectionRecoverySourceStub{page: func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
				return &UpstreamRejectionPendingPage{Items: []UpstreamRejectionReservation{{State: UpstreamRejectionSent, Task: &UpstreamRejectionTaskBinding{TaskID: "active"}}}}, nil
			}},
			resolveKey: func(context.Context, *UpstreamRejectionReservation) (*APIKey, error) { return &APIKey{}, nil },
			reconcile: func(ctx context.Context, r *UpstreamRejectionReservation, key *APIKey) (bool, error) {
				close(entered)
				<-ctx.Done()
				<-release
				return false, ctx.Err()
			},
		}
		w.Start()
		<-entered
		stopped := make(chan struct{})
		go func() { w.Stop(); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while observer was active")
		default:
		}
		close(release)
		<-stopped
	})
}
