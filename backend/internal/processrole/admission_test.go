package processrole

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAdmission(t *testing.T) *AdmissionGate {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admission.json")
	state := AdmissionState{Schema: 1, Revision: 1, Mode: AdmissionLegacy, HoldToken: strings.Repeat("a", 40)}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := OpenAdmissionGate(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.lock.Close() })
	return g
}

func TestAdmissionBarrierWaitsForLegacyAndHeldCreates(t *testing.T) {
	g := newTestAdmission(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	old, err := g.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan error, 1)
	go func() { changed <- g.Transition(ctx, 1, AdmissionHeld) }()
	for !g.Status().Switching {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	newReady := make(chan *AdmissionLease, 1)
	go func() {
		lease, err := g.Begin(ctx)
		if err != nil {
			t.Error(err)
		}
		newReady <- lease
	}()
	select {
	case <-newReady:
		t.Fatal("new writer admitted before legacy POST finished")
	default:
	}
	old.Finish(true)
	old.Finish(true)
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	held := <-newReady
	if held == nil || held.Mode != AdmissionHeld || held.HoldToken != g.Status().HoldToken {
		t.Fatal("new request did not join held batch")
	}
	short, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	defer stop()
	if err := g.Transition(short, 2, AdmissionEnabled); err == nil {
		t.Fatal("released batch while a held Create is in flight")
	}
	held.Finish(true)
	if err := g.Transition(ctx, 2, AdmissionEnabled); err != nil {
		t.Fatal(err)
	}
	status := g.Status()
	if status.PendingLegacy != 0 || status.Active != 0 || status.Revision != 3 {
		t.Fatalf("invalid final state: %+v", status)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAdmissionGate(g.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Status().Mode != AdmissionEnabled {
		t.Fatal("restart resurrected legacy admission")
	}
	if err := reopened.Transition(ctx, 3, AdmissionLegacy); err == nil {
		t.Fatal("returned to unsafe mixed writers")
	}
}

func TestAdmissionRepeatedHandoffWaitsForWritersAndPreviousBatch(t *testing.T) {
	g := newTestAdmission(t)
	ctx := context.Background()
	remaining := 0
	inspect := func(token string) (int, error) {
		if token != strings.Repeat("a", 40) {
			t.Fatal("inventory inspected another hold token")
		}
		return remaining, nil
	}
	New(Config{Role: Serving}).AttachAdmission(g, nil, inspect)
	if err := g.Transition(ctx, 1, AdmissionHeld); err != nil {
		t.Fatal(err)
	}
	if err := g.Transition(ctx, 2, AdmissionEnabled); err != nil {
		t.Fatal(err)
	}
	previousRelease := uint64(3)
	for cycle := 0; cycle < 2; cycle++ {
		revision := g.Status().Revision
		writer, err := g.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		err = g.Transition(short, revision, AdmissionHeld)
		cancel()
		if err == nil || g.Status().Mode != AdmissionEnabled {
			t.Fatal("pause passed an active writer")
		}
		writer.Finish(true)
		if err := g.Transition(ctx, revision, AdmissionHeld); err != nil {
			t.Fatal(err)
		}
		lease, err := g.Begin(ctx)
		if err != nil || lease.Mode != AdmissionHeld {
			t.Fatalf("new request not held: %v", err)
		}
		remaining++
		lease.Finish(true)
		if err := g.Transition(ctx, revision+1, AdmissionEnabled); err != nil {
			t.Fatal(err)
		}
		if err := g.Transition(ctx, revision+2, AdmissionHeld); err == nil {
			t.Fatal("unfinished previous hold was hidden")
		}
		called := false
		if _, err := g.Release(previousRelease, g.Status().HoldToken, func(string) (int, error) {
			called = true
			return 0, nil
		}); err == nil || called {
			t.Fatal("stale release reached the store")
		}
		if n, err := g.Release(revision+2, g.Status().HoldToken, func(string) (int, error) {
			n := remaining
			remaining = 0
			return n, nil
		}); err != nil || n != 1 {
			t.Fatalf("release failed: %d %v", n, err)
		}
		previousRelease = revision + 2
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		g, err = OpenAdmissionGate(g.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = g.lock.Close() })
		New(Config{Role: Serving}).AttachAdmission(g, nil, inspect)
	}
}

func TestAdmissionRepeatedPauseFailsClosedOnInventoryError(t *testing.T) {
	g := newTestAdmission(t)
	ctx := context.Background()
	if err := g.Transition(ctx, 1, AdmissionHeld); err != nil {
		t.Fatal(err)
	}
	if err := g.Transition(ctx, 2, AdmissionEnabled); err != nil {
		t.Fatal(err)
	}
	for _, inspect := range []func(string) (int, error){nil,
		func(string) (int, error) { return 0, errors.New("database unavailable") },
		func(string) (int, error) { return -1, nil },
	} {
		New(Config{Role: Serving}).AttachAdmission(g, nil, inspect)
		if err := g.Transition(ctx, 3, AdmissionHeld); err == nil {
			t.Fatal("unverified empty batch allowed another handoff")
		}
		if g.Status().Revision != 3 || g.Status().Mode != AdmissionEnabled {
			t.Fatal("failed read changed durable admission")
		}
	}
}

func TestAdmissionUnknownLegacyResponseSurvivesRestart(t *testing.T) {
	g := newTestAdmission(t)
	lease, err := g.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Finish(false)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAdmissionGate(g.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Status().PendingLegacy != 1 {
		t.Fatal("ambiguous accepted request was forgotten")
	}
	if err := reopened.Transition(context.Background(), 1, AdmissionHeld); err == nil {
		t.Fatal("unknown paid outcome did not block handoff")
	}
	lease, err = reopened.Begin(context.Background())
	if err != nil {
		t.Fatal("known old owner should still accept independent requests", err)
	}
	lease.Finish(true)
	if reopened.Status().PendingLegacy != 1 {
		t.Fatal("new success erased older ambiguity")
	}
}

func TestAdmissionJournalExcludesSecondServingAndRetainsCrashIntent(t *testing.T) {
	g := newTestAdmission(t)
	if another, err := OpenAdmissionGate(g.path); err == nil {
		_ = another.Close()
		t.Fatal("two proxy owners admitted")
	}
	if _, err := g.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A crashed serving process releases flock but cannot acknowledge its POST.
	if err := g.lock.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAdmissionGate(g.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Status().PendingLegacy != 1 {
		t.Fatal("crash lost pending intent")
	}
	if err := reopened.Transition(context.Background(), 1, AdmissionHeld); err == nil {
		t.Fatal("crash allowed unchecked producer handoff")
	}
}

func TestUnknownAdmissionPreventsRuntimeRetirement(t *testing.T) {
	g := newTestAdmission(t)
	r := New(Config{Role: Serving})
	r.AttachAdmission(g, nil, nil)
	lease, err := g.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Finish(false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if r.Wait(ctx) == nil {
		t.Fatal("unknown legacy request allowed retirement")
	}
}

func TestHeldReleaseRequiresCompletedBarrierAndExactBatch(t *testing.T) {
	g := newTestAdmission(t)
	ctx := context.Background()
	token := g.Status().HoldToken
	calls := 0
	release := func(actual string) (int, error) {
		calls++
		if actual != token {
			t.Fatal("wrong hold batch")
		}
		return 1, nil
	}
	if _, err := g.Release(1, token, release); err == nil {
		t.Fatal("released while legacy writer active")
	}
	if err := g.Transition(ctx, 1, AdmissionHeld); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Release(2, token, release); err == nil {
		t.Fatal("released while more held tasks can be created")
	}
	if err := g.Transition(ctx, 2, AdmissionEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Release(2, token, release); err == nil {
		t.Fatal("stale transition revision allowed")
	}
	if _, err := g.Release(3, strings.Repeat("b", 40), release); err == nil {
		t.Fatal("foreign held batch allowed")
	}
	if count, err := g.Release(3, token, release); err != nil || count != 1 || calls != 1 {
		t.Fatal("valid bounded release did not run once")
	}
}
