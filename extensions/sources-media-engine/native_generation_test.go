package mediaengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type nativeGenerator struct {
	provider
	calls     int
	failure   error
	remaining time.Duration
}

func (p *nativeGenerator) Generate(ctx context.Context, _ *Task) ([]string, error) {
	p.calls++
	if deadline, ok := ctx.Deadline(); ok {
		p.remaining = time.Until(deadline)
	}
	if p.failure != nil {
		return nil, p.failure
	}
	return []string{"data:image/png;base64,aW1hZ2U="}, nil
}
func nativeSetup(t *testing.T) (*Engine, *Store, *boundary, *nativeGenerator, string, []byte) {
	t.Helper()
	old, s, b, _, dir, key := setup(t)
	old.Close()
	s.engineClaimed.Store(false)
	p := &nativeGenerator{}
	e, err := New(s, b, p)
	if err != nil {
		t.Fatal(err)
	}
	return e, s, b, p, dir, key
}
func TestNativeGenerationPersistsBeforeSettlementAndLegacyRecovery(t *testing.T) {
	e, s, b, p, dir, key := nativeSetup(t)
	task := create(t, e)
	steps(t, e, task.ID, 2)
	got, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "settling" || got.UpstreamID != "" || len(got.SourceURLs) != 1 || !strings.HasPrefix(got.SourceURLs[0], "data:") || p.calls != 1 || p.posts != 0 {
		t.Fatalf("native result not durable before billing: phase=%s calls=%d legacy_posts=%d", got.Phase, p.calls, p.posts)
	}
	if b.settled[task.ID] {
		t.Fatal("settled before result persisted")
	}
	e.Close()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// The previous provider contract can finish an already-generated result.
	legacy := &provider{status: "completed"}
	recovered, err := New(reopened, b, legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	steps(t, recovered, task.ID, 2)
	got, _ = reopened.Get(task.ID)
	if got.Phase != "completed" || !b.settled[task.ID] || b.released[task.ID] || legacy.posts != 0 {
		t.Fatal("result recovery resubmitted or lost settlement")
	}
}
func TestNativeGenerationUnknownNeverResubmits(t *testing.T) {
	e, s, b, p, dir, key := nativeSetup(t)
	p.failure = errors.New("response lost")
	task := create(t, e)
	steps(t, e, task.ID, 3)
	got, _ := s.Get(task.ID)
	if got.Phase != "unknown" || p.calls != 1 || p.posts != 0 || b.released[task.ID] || b.settled[task.ID] {
		t.Fatal("uncertain generation must retain original hold without retry")
	}
	e.Close()
	s.Close()
	reopened, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumed, err := New(reopened, b, p)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	steps(t, resumed, task.ID, 2)
	if p.calls != 1 {
		t.Fatal("restart replayed a paid generation")
	}
}
func TestNativeGenerationRejectsAndDefersWithoutLegacyFallback(t *testing.T) {
	for _, cause := range []error{ErrRejected, ErrDeferred} {
		t.Run(cause.Error(), func(t *testing.T) {
			e, s, b, p, _, _ := nativeSetup(t)
			defer e.Close()
			p.failure = cause
			task := create(t, e)
			steps(t, e, task.ID, 2)
			got, _ := s.Get(task.ID)
			if p.calls != 1 || p.posts != 0 {
				t.Fatal("native path fell through to legacy submit")
			}
			if errors.Is(cause, ErrRejected) {
				if got.Phase != "releasing" {
					t.Fatal(got.Phase)
				}
				steps(t, e, task.ID, 1)
				if !b.released[task.ID] || b.settled[task.ID] {
					t.Fatal("rejected generation not released exactly once")
				}
			} else {
				if got.Phase != "queued" {
					t.Fatal(got.Phase)
				}
				p.failure = nil
				steps(t, e, task.ID, 3)
				if !b.settled[task.ID] || b.released[task.ID] || p.calls != 2 {
					t.Fatal("pre-submit deferral failed to resume safely")
				}
			}
		})
	}
}

func TestNativeGenerationHasItsOwnBoundedTimeout(t *testing.T) {
	e, _, _, p, _, _ := nativeSetup(t)
	defer e.Close()
	e.opts.OperationTimeout = time.Millisecond
	e.opts.GenerationTimeout = time.Minute
	task := create(t, e)
	steps(t, e, task.ID, 2)
	if p.remaining < 30*time.Second || p.remaining > time.Minute {
		t.Fatal("generation inherited the short polling timeout", p.remaining)
	}
}
func TestNativeProviderStillPollsPreviouslyAcceptedRemoteTasks(t *testing.T) {
	e, s, b, p, _, _ := nativeSetup(t)
	defer e.Close()
	p.status = "completed"
	task := create(t, e)
	steps(t, e, task.ID, 1)
	task, _ = s.Get(task.ID)
	task.Phase = "running"
	task.UpstreamID = "previous_remote_task"
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 3)
	task, _ = s.Get(task.ID)
	if task.Phase != "completed" || !b.settled[task.ID] || p.calls != 0 || p.posts != 0 {
		t.Fatal("legacy remote task was regenerated or lost")
	}
}
