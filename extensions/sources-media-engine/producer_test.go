package mediaengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProducerPreservesLiveOwnerAndCannotExecute(t *testing.T) {
	e, ownerStore, b, provider, dir, key := setup(t)
	task := create(t, e)
	task.Phase = "submitting"
	if err := ownerStore.Save(task); err != nil {
		t.Fatal(err)
	}
	s, err := OpenProducer(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := NewProducer(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(task.Owner, task.ID)
	if err != nil || got.Phase != "submitting" || got.Version != task.Version {
		t.Fatalf("producer changed live submit intent: %#v, %v", got, err)
	}
	if _, err := New(s, b, provider); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("producer became an engine: %v", err)
	}
	if _, err := New(ownerStore, b, provider); err == nil {
		t.Fatal("second engine on the same handle could repair the first engine's work")
	}
	if err := s.Save(task); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("producer saved execution state: %v", err)
	}
	if err := s.Retain(time.Now()); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("producer performed retention: %v", err)
	}
	if err := s.saveWithEvent(task, 1, "release", "confirmed operator decision"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("producer changed an operator decision: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir, key, Namespace{}); err == nil {
		_ = other.Close()
		t.Fatal("closing a producer released the engine lock")
	}
	got, err = ownerStore.Get(task.ID)
	if err != nil || got.Phase != "submitting" || got.Version != task.Version {
		t.Fatalf("rejected producer operations mutated task: %#v, %v", got, err)
	}
}

func TestProducerRequiresInitializedStoreAndMatchingKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uninitialized")
	key := []byte(strings.Repeat("a", 32))
	if _, err := OpenProducer(dir, key, Namespace{}); err == nil {
		t.Fatal("producer initialized a missing directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenProducer(dir, key, Namespace{}); err == nil {
		t.Fatal("producer initialized an empty store")
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("producer created a database: %v", err)
	}
	owner, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := OpenProducer(dir, []byte(strings.Repeat("b", 32)), Namespace{}); err == nil {
		t.Fatal("producer accepted a different encryption key")
	}
	producer, err := OpenProducer(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	var version int
	if err := owner.db.QueryRow("SELECT version FROM store_metadata WHERE id=1").Scan(&version); err != nil || version != 1 {
		t.Fatalf("store format changed: %d, %v", version, err)
	}
}

func TestProducerWaitsForSchemaInitializationLock(t *testing.T) {
	_, _, _, _, dir, key := setup(t)
	lock, err := openSchemaLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	opened := make(chan error, 1)
	go func() {
		s, err := OpenProducer(dir, key, Namespace{})
		if err == nil {
			err = s.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("producer skipped an in-progress schema initialization: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not open after schema lock was released")
	}
}

type producerProcessResult struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// The child uses a separate address space and SQLite connection, so admission
// cannot accidentally pass by relying on a process-local Go mutex.
func TestProducerProcessHelper(t *testing.T) {
	dir := os.Getenv("MEDIA_ENGINE_TEST_PRODUCER_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	key := []byte(strings.Repeat("a", 32))
	if owner, err := Open(dir, key, Namespace{}); err == nil {
		_ = owner.Close()
		t.Fatal("another process acquired the engine lock")
	}
	s, err := OpenProducer(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := NewProducer(s, Options{MaxRecordsPerKey: 1})
	if err != nil {
		t.Fatal(err)
	}
	in := input()
	in.IdempotencyKey = os.Getenv("MEDIA_ENGINE_TEST_PRODUCER_IDEM")
	task, err := p.Create(in)
	result := producerProcessResult{}
	if errors.Is(err, ErrCapacity) {
		result.Error = "capacity"
	} else if err != nil {
		t.Fatal(err)
	} else {
		result.ID = task.ID
	}
	raw, _ := json.Marshal(result)
	if err := os.WriteFile(os.Getenv("MEDIA_ENGINE_TEST_PRODUCER_RESULT"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProducerAdmissionAcrossProcesses(t *testing.T) {
	for _, sameRequest := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_request=%t", sameRequest), func(t *testing.T) {
			_, owner, _, _, dir, _ := setup(t)
			const writers = 6
			var wg sync.WaitGroup
			results := make(chan producerProcessResult, writers)
			start := make(chan struct{})
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					idem := "shared"
					if !sameRequest {
						idem = fmt.Sprintf("request-%d", i)
					}
					resultPath := filepath.Join(dir, fmt.Sprintf("child-%d.json", i))
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProducerProcessHelper$", "-test.count=1")
					cmd.Env = append(os.Environ(), "MEDIA_ENGINE_TEST_PRODUCER_DIR="+dir, "MEDIA_ENGINE_TEST_PRODUCER_IDEM="+idem, "MEDIA_ENGINE_TEST_PRODUCER_RESULT="+resultPath)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Errorf("producer process failed: %v\n%s", err, out)
						return
					}
					raw, err := os.ReadFile(resultPath)
					var result producerProcessResult
					if err != nil || json.Unmarshal(raw, &result) != nil {
						t.Errorf("invalid producer result: %v", err)
						return
					}
					results <- result
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			id := ""
			created, capacity := 0, 0
			for result := range results {
				if result.Error == "capacity" {
					capacity++
					continue
				}
				if result.ID == "" || (sameRequest && id != "" && id != result.ID) {
					t.Fatalf("duplicate or missing durable admission: %q vs %q", id, result.ID)
				}
				id = result.ID
				created++
			}
			if sameRequest && (created != writers || capacity != 0) {
				t.Fatalf("idempotent retry lost to capacity: created=%d capacity=%d", created, capacity)
			}
			if !sameRequest && (created != 1 || capacity != writers-1) {
				t.Fatalf("capacity exceeded: created=%d capacity=%d", created, capacity)
			}
			count, err := owner.ActiveCount(input().Owner.KeyID)
			if err != nil || count != 1 {
				t.Fatalf("durable active count = %d: %v", count, err)
			}
			if sameRequest {
				p, err := NewProducer(owner, Options{MaxRecordsPerKey: 1})
				if err != nil {
					t.Fatal(err)
				}
				in := input()
				in.IdempotencyKey = "shared"
				in.PayloadHash = strings.Repeat("b", 64)
				if _, err := p.Create(in); !errors.Is(err, ErrConflict) {
					t.Fatalf("conflicting retry was accepted: %v", err)
				}
			}
		})
	}
}

type drainingProvider struct {
	entered chan struct{}
	finish  chan struct{}
	posts   atomic.Int32
}

func (p *drainingProvider) Submit(ctx context.Context, _ Credential, _ json.RawMessage, _ string) (string, error) {
	p.posts.Add(1)
	close(p.entered)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-p.finish:
		return "vendor_drain_1", nil
	}
}
func (p *drainingProvider) Poll(context.Context, Credential, string) (Poll, error) {
	return Poll{Status: "processing"}, nil
}

func TestGracefulOwnerHandoffPreservesSubmitOutcome(t *testing.T) {
	e, s, b, _, dir, key := setup(t)
	p := &drainingProvider{entered: make(chan struct{}), finish: make(chan struct{})}
	e.provider = p
	e.opts.OperationTimeout = 5 * time.Second
	task := create(t, e)
	steps(t, e, task.ID, 1)
	e.Start()
	t.Cleanup(e.Close)
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background engine did not begin Submit")
	}
	in := input()
	in.IdempotencyKey = "queued-for-next-owner"
	waiting, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { e.CloseGracefully(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("graceful close interrupted an in-flight Submit")
	case <-time.After(30 * time.Millisecond):
	}
	if other, err := Open(dir, key, Namespace{}); err == nil {
		_ = other.Close()
		t.Fatal("engine lock released before Submit outcome was persisted")
	}
	close(p.finish)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("graceful close did not finish after the provider returned")
	}
	got, err := s.Get(task.ID)
	if err != nil || got.Phase != "running" || got.UpstreamID != "vendor_drain_1" {
		t.Fatalf("submit outcome was lost: %#v, %v", got, err)
	}
	if err := e.Step(context.Background(), waiting.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(waiting.ID)
	if err != nil || got.Phase != "admitting" {
		t.Fatalf("draining engine started another task: %#v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	nextStore, err := Open(dir, key, Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer nextStore.Close()
	next, err := New(nextStore, b, p)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, next, task.ID, 1)
	if p.posts.Load() != 1 {
		t.Fatal("next owner repeated the provider POST")
	}
}

func TestImmediateCloseKeepsCancellationSemantics(t *testing.T) {
	e, s, _, _, _, _ := setup(t)
	p := &drainingProvider{entered: make(chan struct{}), finish: make(chan struct{})}
	e.provider = p
	task := create(t, e)
	steps(t, e, task.ID, 1)
	e.Start()
	t.Cleanup(e.Close)
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background engine did not begin Submit")
	}
	e.Close()
	got, err := s.Get(task.ID)
	if err != nil || got.Phase != "unknown" {
		t.Fatalf("immediate cancellation did not preserve an unknown outcome: %#v, %v", got, err)
	}
}
