package mediaengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type boundary struct {
	mu                          sync.Mutex
	reserved, settled, released map[string]bool
	failSettle                  bool
}

func (b *boundary) Reserve(_ context.Context, t *Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved[t.ID] = true
	return nil
}
func (b *boundary) Release(_ context.Context, t *Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released[t.ID] = true
	return nil
}
func (b *boundary) Settle(_ context.Context, t *Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failSettle {
		return errors.New("temporary database outage")
	}
	b.settled[t.ID] = true
	return nil
}
func (b *boundary) StoreResult(context.Context, *Task, []string) (json.RawMessage, error) {
	return json.RawMessage(`{"data":[{"url":"https://owned.example/image.png"}]}`), nil
}

type provider struct {
	posts     int
	submitErr bool
	status    string
}

func (p *provider) Submit(context.Context, Credential, json.RawMessage, string) (string, error) {
	p.posts++
	if p.submitErr {
		return "", errors.New("disconnected")
	}
	return "vendor_1", nil
}
func (p *provider) Poll(context.Context, Credential, string) (Poll, error) {
	return Poll{Status: p.status, URLs: []string{"https://vendor.example/image.png"}}, nil
}
func setup(t *testing.T) (*Engine, *Store, *boundary, *provider, string, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	key := []byte(strings.Repeat("a", 32))
	s, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b := &boundary{reserved: map[string]bool{}, settled: map[string]bool{}, released: map[string]bool{}}
	p := &provider{status: "completed"}
	e, err := New(s, b, p)
	if err != nil {
		t.Fatal(err)
	}
	return e, s, b, p, dir, key
}
func input() Input {
	body := json.RawMessage(`{"model":"gpt-image-2","prompt":"synthetic"}`)
	h := sha256.Sum256(body)
	return Input{Owner: Owner{1, 2, 74}, IdempotencyKey: "same-request", PayloadHash: hex.EncodeToString(h[:]), Body: body, Ticket: json.RawMessage(`{"cost":0.03}`), Credential: Credential{APIKey: "private-credential", BaseURL: "https://vendor.example", AccountID: 1}}
}
func create(t *testing.T, e *Engine) *Task {
	t.Helper()
	task, err := e.Create(input())
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func steps(t *testing.T, e *Engine, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := e.Step(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDurableSuccessfulTask(t *testing.T) {
	e, s, b, p, dir, key := setup(t)
	task := create(t, e)
	steps(t, e, task.ID, 2)
	if p.posts != 1 {
		t.Fatal(p.posts)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(filepath.Join(dir, "tasks.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, wal...)
	if strings.Contains(string(raw), "private-credential") {
		t.Fatal("credential was persisted in plaintext")
	}
	_ = s.Close()
	reopened, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e, err = New(reopened, b, p)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 4)
	out, _ := e.Get(input().Owner, task.ID)
	if out.Phase != "completed" || len(b.settled) != 1 || p.posts != 1 {
		t.Fatalf("phase %s, settlements %d, posts %d", out.Phase, len(b.settled), p.posts)
	}
}
func TestUnknownSubmitNeverReposts(t *testing.T) {
	e, s, b, p, dir, key := setup(t)
	p.submitErr = true
	task := create(t, e)
	steps(t, e, task.ID, 2)
	_ = s.Close()
	s2, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	e, err = New(s2, b, p)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 5)
	out, _ := e.Get(input().Owner, task.ID)
	if out.Phase != "unknown" || p.posts != 1 || len(b.settled) != 0 || len(b.released) != 0 {
		t.Fatal("ambiguous task was retried or financially finalized")
	}
}
func TestCrashAfterSubmitIntentNeverPosts(t *testing.T) {
	e, s, b, p, dir, key := setup(t)
	task := create(t, e)
	task.Phase = "submitting"
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	e, err = New(s2, b, p)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 2)
	out, _ := e.Get(input().Owner, task.ID)
	if out.Phase != "unknown" || p.posts != 0 {
		t.Fatal("crashed submit was replayed")
	}
}
func TestSettlementRetriesWithoutAnotherGeneration(t *testing.T) {
	e, _, b, p, _, _ := setup(t)
	task := create(t, e)
	steps(t, e, task.ID, 3)
	b.failSettle = true
	if e.Step(context.Background(), task.ID) == nil {
		t.Fatal("expected settlement outage")
	}
	b.failSettle = false
	steps(t, e, task.ID, 3)
	if p.posts != 1 || len(b.settled) != 1 {
		t.Fatal("generation or settlement duplicated")
	}
}
func TestFailureReleasesOnlyOnce(t *testing.T) {
	e, _, b, p, _, _ := setup(t)
	p.status = "failed"
	task := create(t, e)
	steps(t, e, task.ID, 6)
	out, _ := e.Get(input().Owner, task.ID)
	if out.Phase != "failed" || len(b.released) != 1 || len(b.settled) != 0 {
		t.Fatal("failed task charged")
	}
}
func TestConcurrentIdempotencyAndOwnership(t *testing.T) {
	e, _, _, _, _, _ := setup(t)
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := e.Create(input())
			if err != nil {
				t.Error(err)
				return
			}
			ids <- task.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for got := range ids {
		if id != "" && id != got {
			t.Fatal("duplicate task")
		}
		id = got
	}
	bad := input()
	bad.PayloadHash = "different"
	if _, err := e.Existing(bad.Owner, bad.IdempotencyKey, bad.PayloadHash); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := e.Get(Owner{3, 4, 74}, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-user access")
	}
}
func TestSingleWriterAndWrongKey(t *testing.T) {
	_, s, _, _, dir, _ := setup(t)
	if _, err := Open(dir, []byte(strings.Repeat("a", 32)), Namespace{}); err == nil {
		t.Fatal("second writer")
	}
	task := &Task{ID: "mediatask_" + strings.Repeat("f", 32), Phase: "running"}
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(dir, []byte(strings.Repeat("b", 32)), Namespace{}); err == nil {
		t.Fatal("wrong decryption key accepted")
	}
}
