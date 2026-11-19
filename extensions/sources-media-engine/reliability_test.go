package mediaengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fairProvider struct {
	mu     sync.Mutex
	seen   map[string]int
	result string
	err    error
}

func (p *fairProvider) Submit(_ context.Context, _ Credential, _ json.RawMessage, id string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[id]++
	return "up_" + id, p.err
}
func (p *fairProvider) Poll(context.Context, Credential, string) (Poll, error) {
	return Poll{Status: p.result, URLs: []string{"https://vendor.example/image"}}, nil
}
func TestDuePaginationDoesNotStarveBeyondFirstPage(t *testing.T) {
	e, _, b, _, _, _ := setup(t)
	now := time.Now()
	e.opts.Now = func() time.Time { return now }
	e.opts.Workers = 2
	p := &fairProvider{seen: map[string]int{}, result: "processing"}
	e.provider = p
	for i := 0; i < 35; i++ {
		in := input()
		in.IdempotencyKey = fmt.Sprint(i)
		if _, err := e.Create(in); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		e.Sweep(context.Background())
		now = now.Add(time.Second)
	}
	if len(p.seen) != 35 || len(b.reserved) != 35 {
		t.Fatalf("processed %d/35 tasks, reserved %d", len(p.seen), len(b.reserved))
	}
	for _, count := range p.seen {
		if count != 1 {
			t.Fatal("POST duplicated")
		}
	}
}
func TestStoreCASAndUniqueUpstreamBinding(t *testing.T) {
	e, s, _, _, _, _ := setup(t)
	a := create(t, e)
	stale := clone(a)
	a.Phase = "queued"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.Save(stale), ErrConflict) {
		t.Fatal("stale writer accepted")
	}
	a.UpstreamID = "vendor_1"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	in := input()
	in.IdempotencyKey = "second"
	b, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	b.UpstreamID = "vendor_1"
	if !errors.Is(s.Save(b), ErrConflict) {
		t.Fatal("two tasks claim one upstream result")
	}
	got, err := s.Get(b.ID)
	if err != nil || got.UpstreamID != "" {
		t.Fatal("failed CAS transaction was partially saved")
	}
	a.UpstreamID = "vendor_2"
	if !errors.Is(s.Save(a), ErrConflict) {
		t.Fatal("upstream binding was replaced")
	}
}
func TestReviewExtendsDeadlineWithoutRewritingCreatedAt(t *testing.T) {
	e, s, _, _, _, _ := setup(t)
	task := create(t, e)
	original := task.CreatedAt
	task.Phase = "delivery_review"
	task.UpstreamID = "vendor_1"
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(original, 0).Add(48 * time.Hour)
	e.opts.Now = func() time.Time { return now }
	if err := e.Resolve(context.Background(), task.ID, "retry_delivery", "", 7, "confirmed delivery retry"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(task.ID)
	if got.CreatedAt != original || got.ReviewAfterAt != now.Add(e.opts.ReviewAfter).Unix() {
		t.Fatal("review mutated historical timestamp")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM task_events WHERE operator_id=7 AND action='retry_delivery'").Scan(&count); err != nil || count != 1 {
		t.Fatal("missing audit")
	}
}
func TestRetentionKeepsUnknownMoneyAndIdempotencyTombstone(t *testing.T) {
	e, s, _, _, _, _ := setup(t)
	now := time.Now()
	done := create(t, e)
	done.Phase = "completed"
	done.TerminalAt = now.Add(-8 * 24 * time.Hour).Unix()
	if err := s.Save(done); err != nil {
		t.Fatal(err)
	}
	in := input()
	in.IdempotencyKey = "unknown"
	unknown, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	unknown.Phase = "unknown"
	unknown.CreatedAt = now.Add(-50 * 24 * time.Hour).Unix()
	if err = s.Save(unknown); err != nil {
		t.Fatal(err)
	}
	if err = s.Retain(now); err != nil {
		t.Fatal(err)
	}
	archived, err := s.Existing(input().Owner, input().IdempotencyKey)
	if err != nil || archived.Phase != "archived" || len(archived.Ticket) != 0 || archived.Credential.APIKey != "" {
		t.Fatal("terminal payload not archived")
	}
	pending, err := s.Get(unknown.ID)
	if err != nil || len(pending.Ticket) == 0 {
		t.Fatal("unresolved money lost")
	}
}
func TestDeferredDoesNotBecomeUnknownOrSpend(t *testing.T) {
	e, _, b, _, _, _ := setup(t)
	p := &fairProvider{seen: map[string]int{}, err: ErrDeferred}
	e.provider = p
	task := create(t, e)
	steps(t, e, task.ID, 2)
	got, _ := e.Get(task.Owner, task.ID)
	if got.Phase != "queued" || len(b.settled) != 0 || len(b.released) != 0 {
		t.Fatal("capacity became a financial outcome")
	}
}
func TestLookupRejectsOtherGroup(t *testing.T) {
	e, _, _, _, _, _ := setup(t)
	task := create(t, e)
	owner := task.Owner
	owner.GroupID++
	if _, err := e.Get(owner, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-group lookup")
	}
}

func TestPersistedSubmitIntentRecoversWithoutProcessRestart(t *testing.T) {
	e, s, _, p, _, _ := setup(t)
	task := create(t, e)
	steps(t, e, task.ID, 1)
	_, err := s.db.Exec(`CREATE TRIGGER fail_outcome BEFORE UPDATE ON tasks WHEN NEW.phase IN ('running','unknown') BEGIN SELECT RAISE(ABORT,'synthetic storage failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Step(context.Background(), task.ID); err == nil {
		t.Fatal("expected failure")
	}
	saved, err := s.Get(task.ID)
	if err != nil || saved.Phase != "submitting" {
		t.Fatal("submit intent not durable")
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_outcome`); err != nil {
		t.Fatal(err)
	}
	e.Sweep(context.Background())
	saved, _ = s.Get(task.ID)
	if saved.Phase != "unknown" || p.posts != 1 {
		t.Fatalf("phase=%s posts=%d", saved.Phase, p.posts)
	}
}

func TestExpiredQueueReleasesOnlyKnownUnsubmittedHold(t *testing.T) {
	e, _, b, p, _, _ := setup(t)
	task := create(t, e)
	steps(t, e, task.ID, 1)
	now := time.Unix(task.CreatedAt, 0).Add(25 * time.Hour)
	e.opts.Now = func() time.Time { return now }
	steps(t, e, task.ID, 2)
	got, _ := e.Get(task.Owner, task.ID)
	if got.Phase != "failed" || len(b.released) != 1 || p.posts != 0 {
		t.Fatalf("phase=%s released=%d posts=%d", got.Phase, len(b.released), p.posts)
	}
}

func TestProviderConcurrencyIsDurableAndSeparateFromRecordCapacity(t *testing.T) {
	e, _, _, _, _, _ := setup(t)
	p := &fairProvider{seen: map[string]int{}, result: "processing"}
	e.provider = p
	in := input()
	in.Credential.Concurrency = 1
	a, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	in.IdempotencyKey = "second"
	b, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, a.ID, 2)
	steps(t, e, b.ID, 2)
	pending, _ := e.Get(b.Owner, b.ID)
	if pending.Phase != "queued" || len(p.seen) != 1 {
		t.Fatal("provider concurrency was mistaken for HTTP request concurrency")
	}
	// A completed upstream frees capacity; local settlement/delivery need not block it.
	p.result = "completed"
	steps(t, e, a.ID, 1)
	steps(t, e, b.ID, 1)
	if len(p.seen) != 2 {
		t.Fatal("completed generation did not release provider slot")
	}
}
