package mediaengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type repeatedHoldProvider struct{ *provider }

func (p repeatedHoldProvider) Submit(ctx context.Context, credential Credential, body json.RawMessage, idempotency string) (string, error) {
	ref, err := p.provider.Submit(ctx, credential, body, idempotency)
	return fmt.Sprintf("%s_%d", ref, p.posts), err
}

func TestRepeatedAdmissionHoldInventoryDoesNotReleaseOrCharge(t *testing.T) {
	e, owner, billing, vendor, dir, key := setup(t)
	e.provider = repeatedHoldProvider{vendor}
	defer owner.Close()
	defer e.CloseGracefully()
	store, err := OpenProducer(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p, err := NewProducer(store, Options{MaxRecordsPerKey: 10})
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("c", 40)
	for cycle := 0; cycle < 2; cycle++ {
		in := input()
		in.IdempotencyKey = fmt.Sprintf("repeated-handoff-%d", cycle)
		task, err := p.CreateHeld(in, token)
		if err != nil {
			t.Fatal(err)
		}
		for probe := 0; probe < 2; probe++ {
			if count, err := p.AdmissionHoldCount(token); err != nil || count != 1 {
				t.Fatalf("hold inventory: %d %v", count, err)
			}
			e.Sweep(context.Background())
			if vendor.posts != cycle || len(billing.reserved) != cycle {
				t.Fatal("inventory or sweep executed held work")
			}
		}
		if n, err := p.ReleaseAdmissionHold(token); err != nil || n != 1 {
			t.Fatalf("release: %d %v", n, err)
		}
		if count, err := p.AdmissionHoldCount(token); err != nil || count != 0 {
			t.Fatalf("remaining hold: %d %v", count, err)
		}
		steps(t, e, task.ID, 5)
		if vendor.posts != cycle+1 || len(billing.settled) != cycle+1 {
			t.Fatal("repeated handoff did not settle exactly once")
		}
	}
	if _, err := p.AdmissionHoldCount("invalid"); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid token accepted")
	}
}

func TestAdmissionHoldDurableUntilExplicitTokenRelease(t *testing.T) {
	e, owner, b, vendor, dir, key := setup(t)
	s, err := OpenProducer(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := NewProducer(s, Options{MaxRecordsPerKey: 1})
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 32)
	task, err := p.CreateHeld(input(), token)
	if err != nil {
		t.Fatal(err)
	}
	if task.Version != 1 || task.Phase != "admitting" || task.AdmissionHold != token || task.NextRun != admissionHoldNextRun(token) {
		t.Fatalf("invalid admission hold: %#v", task)
	}
	if task.NextRun < 1<<62 || task.NextRun > math.MaxInt64 {
		t.Fatal("sentinel outside reserved future range")
	}
	for _, create := range []func(Input) (*Task, error){p.Create, func(in Input) (*Task, error) { return p.CreateHeld(in, strings.Repeat("b", 64)) }} {
		retry, err := create(input())
		if err != nil || retry.ID != task.ID || retry.AdmissionHold != token || retry.Version != 1 {
			t.Fatalf("retry changed the original hold: %#v %v", retry, err)
		}
	}
	other := input()
	other.IdempotencyKey = "another"
	if _, err := p.CreateHeld(other, token); !errors.Is(err, ErrCapacity) {
		t.Fatalf("held admission bypassed capacity: %v", err)
	}
	if err := e.Step(context.Background(), task.ID); !errors.Is(err, ErrDeferred) {
		t.Fatalf("direct Step bypassed hold: %v", err)
	}
	e.Sweep(context.Background())
	if vendor.posts != 0 || len(b.reserved) != 0 {
		t.Fatal("hold performed provider/billing work")
	}
	if n, err := p.ReleaseAdmissionHold(strings.Repeat("b", 64)); err != nil || n != 0 {
		t.Fatalf("wrong token selected this hold: %d %v", n, err)
	}
	e.CloseGracefully()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next, err := New(restarted, b, vendor)
	if err != nil {
		t.Fatal(err)
	}
	next.Sweep(context.Background())
	got, err := p.Get(task.Owner, task.ID)
	if err != nil || got.Version != 1 || got.AdmissionHold != token || vendor.posts != 0 {
		t.Fatalf("new owner implicitly repaired/released hold: %#v %v", got, err)
	}
	// An owner still needs the token; a producer can release without owning .lock.
	if n, err := p.ReleaseAdmissionHold(token); err != nil || n != 1 {
		t.Fatalf("explicit release: %d %v", n, err)
	}
	if n, err := p.ReleaseAdmissionHold(token); err != nil || n != 0 {
		t.Fatalf("release was not idempotent: %d %v", n, err)
	}
	got, err = p.Get(task.Owner, task.ID)
	if err != nil || got.Version != 2 || got.AdmissionHold != "" || got.Phase != "admitting" || got.UpstreamID != "" {
		t.Fatalf("release changed more than scheduling: %#v %v", got, err)
	}
	steps(t, next, task.ID, 5)
	got, err = p.Get(task.Owner, task.ID)
	if err != nil || got.Phase != "completed" || vendor.posts != 1 || len(b.reserved) != 1 || len(b.settled) != 1 || len(b.released) != 0 {
		t.Fatalf("released task did not execute once: %#v %v", got, err)
	}
}

func TestAdmissionHoldRejectsInvalidTokensAndStaleVersion(t *testing.T) {
	e, owner, _, _, _, _ := setup(t)
	for _, token := range []string{"", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("0", 65), strings.Repeat("f", 32) + "\n"} {
		if _, err := e.CreateHeld(input(), token); !errors.Is(err, ErrConflict) {
			t.Fatalf("accepted invalid hold token: %v", err)
		}
		if _, err := e.ReleaseAdmissionHold(token); !errors.Is(err, ErrConflict) {
			t.Fatalf("accepted invalid release token: %v", err)
		}
	}
	token := strings.Repeat("c", 64)
	task, err := e.CreateHeld(input(), token)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an unexpected owner edit while retaining the matching sentinel.
	if err := owner.Save(task); err != nil {
		t.Fatal(err)
	}
	if n, err := e.ReleaseAdmissionHold(token); n != 0 || !errors.Is(err, ErrConflict) {
		t.Fatalf("released a non-pristine task: %d %v", n, err)
	}
	got, err := owner.Get(task.ID)
	if err != nil || got.Version != 2 || got.AdmissionHold != token {
		t.Fatalf("failed release mutated task: %#v %v", got, err)
	}
}

func TestAdmissionHoldExactTokenAndWholeBatchRollback(t *testing.T) {
	e, owner, _, _, _, _ := setup(t)
	token := strings.Repeat("a", 32)
	valid, err := e.CreateHeld(input(), token)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic hash-collision/corrupt-index fixture: the authenticated full
	// token disagrees with the selected sentinel. No record in the batch may move.
	foreign := clone(valid)
	foreign.ID = "mediatask_" + strings.Repeat("f", 32)
	foreign.Version = 0
	foreign.IdempotencyKey = "foreign-token"
	foreign.AdmissionHold = strings.Repeat("b", 32)
	if err := owner.Save(foreign); err != nil {
		t.Fatal(err)
	}
	if n, err := e.ReleaseAdmissionHold(token); n != 0 || !errors.Is(err, ErrConflict) {
		t.Fatalf("released token mismatch: %d %v", n, err)
	}
	got, err := owner.Get(valid.ID)
	if err != nil || got.Version != 1 || got.AdmissionHold != token {
		t.Fatalf("part of failed batch committed: %#v %v", got, err)
	}
}

func TestAdmissionHoldReleaseIsBoundedAndDoesNotScanOtherTokens(t *testing.T) {
	e, owner, _, _, _, _ := setup(t)
	token, foreign := strings.Repeat("1", 32), strings.Repeat("2", 64)
	for i := 0; i < 201; i++ {
		in := input()
		in.IdempotencyKey = fmt.Sprintf("held-%03d", i)
		if _, err := e.CreateHeld(in, token); err != nil {
			t.Fatal(err)
		}
	}
	in := input()
	in.IdempotencyKey = "another-hold"
	other, err := e.CreateHeld(in, foreign)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{100, 100, 1, 0} {
		if n, err := e.ReleaseAdmissionHold(token); err != nil || n != want {
			t.Fatalf("release batch = %d, want %d: %v", n, want, err)
		}
	}
	got, err := owner.Get(other.ID)
	if err != nil || got.Version != 1 || got.AdmissionHold != foreign || got.NextRun != admissionHoldNextRun(foreign) {
		t.Fatalf("another hold token was inspected/released: %#v %v", got, err)
	}
}

func TestAdmissionHoldConcurrentReleaseIsCASProtected(t *testing.T) {
	e, _, _, _, dir, key := setup(t)
	token := strings.Repeat("3", 32)
	task, err := e.CreateHeld(input(), token)
	if err != nil {
		t.Fatal(err)
	}
	var producers []*Producer
	for i := 0; i < 2; i++ {
		s, err := OpenProducer(dir, key, Namespace{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		p, err := NewProducer(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		producers = append(producers, p)
	}
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for _, p := range producers {
		wg.Go(func() {
			n, err := p.ReleaseAdmissionHold(token)
			if err != nil {
				t.Error(err)
			}
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	got, err := e.Get(task.Owner, task.ID)
	if total != 1 || err != nil || got.Version != 2 || got.AdmissionHold != "" || got.NextRun > time.Now().Add(time.Second).UnixMilli() {
		t.Fatalf("release was not singular: n=%d %#v %v", total, got, err)
	}
}
