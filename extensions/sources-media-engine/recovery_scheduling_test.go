package mediaengine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recoveryBlockingGenerator struct {
	entered     chan struct{}
	finish      chan struct{}
	enterOnce   sync.Once
	calls       atomic.Int32
	cancelled   atomic.Int32
	posts       atomic.Int32
	pollEntered chan struct{}
	pollFinish  chan struct{}
	pollOnce    sync.Once
	polls       atomic.Int32
}

func (p *recoveryBlockingGenerator) Generate(ctx context.Context, _ *Task) ([]string, error) {
	p.calls.Add(1)
	p.enterOnce.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
		p.cancelled.Add(1)
		return nil, ctx.Err()
	case <-p.finish:
		return []string{"data:image/png;base64,aW1hZ2U="}, nil
	}
}

func (p *recoveryBlockingGenerator) Submit(context.Context, Credential, json.RawMessage, string) (string, error) {
	p.posts.Add(1)
	return "", errors.New("unexpected legacy submit")
}

func (p *recoveryBlockingGenerator) Poll(ctx context.Context, _ Credential, _ string) (Poll, error) {
	p.polls.Add(1)
	if p.pollEntered != nil {
		p.pollOnce.Do(func() { close(p.pollEntered) })
		select {
		case <-ctx.Done():
			return Poll{}, ctx.Err()
		case <-p.pollFinish:
		}
	}
	return Poll{Status: "processing"}, nil
}

type recoveryBlockingBoundary struct {
	*boundary
	blockRelease bool
	entered      chan struct{}
	finish       chan struct{}
	enterOnce    sync.Once
	calls        atomic.Int32
	cancelled    atomic.Int32
}

func (b *recoveryBlockingBoundary) UnknownReleaseAmount(*Task) (int64, error) {
	return 3000000, nil
}

func (b *recoveryBlockingBoundary) Release(ctx context.Context, task *Task) error {
	b.calls.Add(1)
	b.enterOnce.Do(func() { close(b.entered) })
	if b.blockRelease {
		select {
		case <-ctx.Done():
			b.cancelled.Add(1)
			return ctx.Err()
		case <-b.finish:
		}
	}
	return b.boundary.Release(ctx, task)
}

func recoverySchedulingSetup(t *testing.T, blockRelease bool) (*Engine, *Store, *recoveryBlockingBoundary, *recoveryBlockingGenerator, string, string) {
	t.Helper()
	e, store, original, oldProvider, _ := recoverySetup(t)
	e.opts.Now = time.Now
	e.opts.Workers = 1
	e.opts.OperationTimeout = 10 * time.Second
	e.opts.GenerationTimeout = 15 * time.Second
	unknown := unknownTask(t, e, oldProvider)
	unknown.SubmittedAt = time.Now().Add(-61 * time.Minute).Unix()
	unknown.NextRun = time.Now().Add(time.Hour).UnixMilli()
	if err := store.Save(unknown); err != nil {
		t.Fatal(err)
	}
	b := &recoveryBlockingBoundary{boundary: original.boundary, blockRelease: blockRelease, entered: make(chan struct{}), finish: make(chan struct{})}
	p := &recoveryBlockingGenerator{entered: make(chan struct{}), finish: make(chan struct{})}
	e.boundary, e.provider = b, p
	in := input()
	in.IdempotencyKey = "slow-generation"
	slow, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, slow.ID, 1)
	return e, store, b, p, unknown.ID, slow.ID
}

func awaitRecoverySignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for " + description)
	}
}

func makeRecoveryTaskDue(t *testing.T, store *Store, id string) {
	t.Helper()
	task, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	task.NextRun = 0
	if err = store.Save(task); err != nil {
		t.Fatal(err)
	}
}

func awaitRecoveryPhase(t *testing.T, store *Store, id, phase string) *Task {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		task, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Phase == phase {
			return task
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("task phase stayed %s, want %s", task.Phase, phase)
		}
	}
}

func TestRecoveryClosesExpiredUnknownWhileNativeGenerationBlocked(t *testing.T) {
	e, store, b, p, unknownID, slowID := recoverySchedulingSetup(t, false)
	e.Start()
	awaitRecoverySignal(t, p.entered, "blocked Generate")
	makeRecoveryTaskDue(t, store, unknownID)
	closed := awaitRecoveryPhase(t, store, unknownID, "failed")
	slow, err := store.Get(slowID)
	if err != nil {
		t.Fatal(err)
	}
	if slow.Phase != "submitting" || closed.CustomerClosedAt == 0 || closed.SupplierOutcome != "unknown" || b.calls.Load() != 1 || p.calls.Load() != 1 || p.posts.Load() != 0 {
		t.Fatal("recovery waited for generation, duplicated release or reposted an unknown task")
	}
	close(p.finish)
	e.CloseGracefully()
}

func TestRecoveryGracefulCloseWaitsForGenerationAndApprovedRelease(t *testing.T) {
	e, store, b, p, unknownID, slowID := recoverySchedulingSetup(t, true)
	e.Start()
	awaitRecoverySignal(t, p.entered, "blocked Generate")
	makeRecoveryTaskDue(t, store, unknownID)
	awaitRecoverySignal(t, b.entered, "approved release")
	drained := make(chan struct{})
	go func() { e.CloseGracefully(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("graceful close returned before in-flight operations finished")
	case <-time.After(30 * time.Millisecond):
	}
	if b.cancelled.Load() != 0 || p.cancelled.Load() != 0 {
		t.Fatal("graceful close cancelled a claimed operation")
	}
	close(b.finish)
	closed := awaitRecoveryPhase(t, store, unknownID, "failed")
	select {
	case <-drained:
		t.Fatal("release completion skipped the still-running generation drain")
	default:
	}
	close(p.finish)
	awaitRecoverySignal(t, drained, "complete graceful drain")
	slow, err := store.Get(slowID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.CustomerClosedAt == 0 || slow.Phase != "settling" || p.cancelled.Load() != 0 || b.cancelled.Load() != 0 || b.calls.Load() != 1 {
		t.Fatal("graceful drain lost a money or generation outcome")
	}
	in := input()
	in.IdempotencyKey = "next-owner-only"
	waiting, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	e.Sweep(context.Background())
	waiting, err = store.Get(waiting.ID)
	if err != nil || waiting.Phase != "admitting" || p.calls.Load() != 1 {
		t.Fatal("a stopped recovery lane accepted another operation", err)
	}
}

func TestRecoveryImmediateCloseCancelsAllLanesAndPreservesReleaseIntent(t *testing.T) {
	e, store, b, p, unknownID, slowID := recoverySchedulingSetup(t, true)
	e.Start()
	awaitRecoverySignal(t, p.entered, "blocked Generate")
	makeRecoveryTaskDue(t, store, unknownID)
	awaitRecoverySignal(t, b.entered, "approved release")
	stopped := make(chan struct{})
	go func() { e.Close(); close(stopped) }()
	awaitRecoverySignal(t, stopped, "cancelled operations to persist")
	intent, err := store.Get(unknownID)
	if err != nil {
		t.Fatal(err)
	}
	slow, err := store.Get(slowID)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	released := b.released[unknownID]
	b.mu.Unlock()
	if intent.Phase != "releasing" || intent.Failure != automaticUnknownRelease || intent.CustomerClosedAt != 0 || released || slow.Phase != "unknown" || b.cancelled.Load() != 1 || p.cancelled.Load() != 1 || p.calls.Load() != 1 || p.posts.Load() != 0 {
		t.Fatal("immediate close lost a durable intent or falsely claimed customer closure")
	}
}

func TestRecoveryApprovedReleaseDoesNotWaitForSupplierLookup(t *testing.T) {
	e, store, b, p, unknownID, _ := recoverySchedulingSetup(t, false)
	// Authorize this original hold first, then make its money step due only after
	// both unrelated generation and a different supplier lookup are blocked.
	steps(t, e, unknownID, 1)
	intent, err := store.Get(unknownID)
	if err != nil {
		t.Fatal(err)
	}
	intent.NextRun = time.Now().Add(time.Hour).UnixMilli()
	if err = store.Save(intent); err != nil {
		t.Fatal(err)
	}
	in := input()
	in.IdempotencyKey = "known-reference-lookup"
	known, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, known.ID, 1)
	known, _ = store.Get(known.ID)
	known.Phase, known.UpstreamID = "unknown", "known_existing_reference"
	known.SubmittedAt = time.Now().Add(-61 * time.Minute).Unix()
	if err = store.Save(known); err != nil {
		t.Fatal(err)
	}
	p.pollEntered, p.pollFinish = make(chan struct{}), make(chan struct{})
	e.Start()
	awaitRecoverySignal(t, p.entered, "blocked Generate")
	awaitRecoverySignal(t, p.pollEntered, "blocked supplier lookup")
	makeRecoveryTaskDue(t, store, unknownID)
	closed := awaitRecoveryPhase(t, store, unknownID, "failed")
	if closed.CustomerClosedAt == 0 || b.calls.Load() != 1 || p.calls.Load() != 1 || p.polls.Load() != 1 || p.posts.Load() != 0 {
		t.Fatal("supplier lookup starved or duplicated an approved release")
	}
	e.Close()
	known, err = store.Get(known.ID)
	if err != nil || known.Phase != "unknown" || known.Failure == automaticUnknownRelease || b.calls.Load() != 1 {
		t.Fatal("cancelled supplier lookup approved a new financial decision", err)
	}
}
