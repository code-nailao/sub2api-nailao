package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type intelligenceFake struct {
	mu          sync.Mutex
	submits     int
	polls       int
	submitErr   error
	pollErr     error
	submissions []intelligenceSubmission
	result      intelligenceRemoteResult
	entered     chan struct{}
	release     chan struct{}
}

func (f *intelligenceFake) Submit(ctx context.Context, in intelligenceSubmission, operation string) (intelligenceRemoteResult, error) {
	f.mu.Lock()
	f.submits++
	f.submissions = append(f.submissions, in)
	n, err := f.submits, f.submitErr
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	return intelligenceRemoteResult{ID: fmt.Sprintf("remote_task_%08d", n), Status: "running", Benchmark: in.Benchmark}, err
}

func (f *intelligenceFake) Poll(ctx context.Context, id string) (intelligenceRemoteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	result := f.result
	result.ID = id
	if result.Status == "" {
		result.Status = "running"
	}
	return result, f.pollErr
}

func intelligenceFixture(t *testing.T) (*IntelligenceService, *intelligenceFake, *Store, *time.Time) {
	t.Helper()
	owner, err := OpenStore(t.TempDir(), bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	store, err := OpenIntelligenceStore(owner)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewIntelligenceService(store, "https://main.example", testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	fake := &intelligenceFake{}
	svc.provider = fake
	return svc, fake, owner, &now
}

func seedIntelligencePlan(t *testing.T, svc *IntelligenceService, enabled bool) IntelligencePlan {
	t.Helper()
	expires := svc.now().Add(30 * 24 * time.Hour)
	plan := IntelligencePlan{ID: randomID(), GroupID: 17, Revision: 1, Name: "Fixture group", Model: intelligenceModel, Effort: "medium", Enabled: enabled, CandySeconds: 300, PelicanSeconds: 1200, KeyID: 19, OwnerID: 9, KeyStatus: "active", Quota: 200, DailyLimit: 20, ExpiresAt: &expires}
	secret, err := svc.Store.encrypt(plan, "sk-private-monitor-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.change(func(st *intelligenceState) error {
		st.Plans[plan.ID] = intelligencePlanRecord{Plan: plan, Secret: secret, Operation: randomID()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return plan
}

func enqueueIntelligence(t *testing.T, svc *IntelligenceService, plan IntelligencePlan, kind string) IntelligenceRun {
	t.Helper()
	run, err := svc.Enqueue(plan.ID, kind, "fixture-"+randomID(), true)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestIntelligenceDurableResultAndIdempotency(t *testing.T) {
	svc, provider, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	run, err := svc.Enqueue(plan.ID, "candy", "test-operation-001", true)
	if err != nil {
		t.Fatal(err)
	}
	same, err := svc.Enqueue(plan.ID, "candy", "test-operation-001", true)
	if err != nil || same.ID != run.ID {
		t.Fatal("idempotent enqueue did not return original run", err)
	}
	svc.tick()
	if provider.submits != 1 || svc.Store.snapshot().Runs[run.ID].RemoteID == "" {
		t.Fatal("accepted ID must be durable after exactly one submission")
	}
	provider.result.Status, provider.result.Benchmark = "succeeded", "candy"
	provider.result.Candy.Status, provider.result.Candy.Answer = "passed", "最少取出 21 个。"
	duration := int64(12000)
	provider.result.Candy.DurationMS = &duration
	*now = now.Add(6 * time.Second)
	svc.tick()
	row := svc.Store.snapshot().Runs[run.ID]
	if row.Run.Status != "succeeded" || row.Run.Verdict != "passed" || !row.Run.HasResult || *row.Run.DurationMS != duration {
		t.Fatal("completed task metadata not preserved")
	}
	result, err := svc.Store.result(run.ID)
	if err != nil || result.Answer != provider.result.Candy.Answer {
		t.Fatal("completed answer not persisted", err)
	}
	svc.tick()
	if provider.submits != 1 || provider.polls != 1 {
		t.Fatal("completed result was executed again")
	}
	raw, _ := os.ReadFile(svc.Store.path)
	if bytes.Contains(raw, []byte("sk-private-monitor-fixture")) || bytes.Contains(raw, []byte("最少取出")) {
		t.Fatal("secret or large result body leaked into task index")
	}
}

func TestIntelligenceUnknownSubmissionNeverReplayed(t *testing.T) {
	svc, provider, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	provider.submitErr = &intelligenceRemoteError{Code: "request_uncertain"}
	run := enqueueIntelligence(t, svc, plan, "pelican")
	svc.tick()
	*now = now.Add(time.Minute)
	svc.tick()
	if provider.submits != 1 || svc.Store.snapshot().Runs[run.ID].Run.Status != "unknown" {
		t.Fatal("ambiguous paid submission was replayed")
	}
	reopened, err := NewIntelligenceService(svc.Store, "https://main.example", testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	reopened.provider, reopened.now = provider, svc.now
	reopened.tick()
	if provider.submits != 1 {
		t.Fatal("restart replayed unknown submission")
	}
}

func TestIntelligenceRestartResumesOnlyKnownRemoteID(t *testing.T) {
	svc, provider, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	known := enqueueIntelligence(t, svc, plan, "candy")
	svc.tick()
	uncertain := enqueueIntelligence(t, svc, plan, "pelican")
	if err := svc.Store.change(func(st *intelligenceState) error {
		r := st.Runs[uncertain.ID]
		r.Run.Status = "submitting"
		st.Runs[uncertain.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIntelligenceService(svc.Store, "https://main.example", testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	reopened.provider, reopened.now = provider, svc.now
	provider.result.Status = "succeeded"
	provider.result.Candy.Status, provider.result.Candy.Answer = "incorrect", "29"
	*now = now.Add(6 * time.Second)
	reopened.tick()
	st := svc.Store.snapshot()
	if st.Runs[uncertain.ID].Run.Status != "unknown" || st.Runs[known.ID].Run.Verdict != "incorrect" || provider.submits != 1 || provider.polls != 1 {
		t.Fatal("restart did not distinguish known and unknown tasks")
	}
}

func TestIntelligenceMissingTaskAndQuota(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
		stop bool
	}{
		{"expired remote task", &intelligenceRemoteError{Code: "task_unavailable", Definitive: true}, "unknown", false},
		{"quota exhausted", nil, "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider, _, now := intelligenceFixture(t)
			plan := seedIntelligencePlan(t, svc, false)
			run := enqueueIntelligence(t, svc, plan, "candy")
			svc.tick()
			provider.pollErr = tc.err
			provider.result.Status = "failed"
			provider.result.Error = json.RawMessage(`"insufficient_quota private-api.example sk-never-display"`)
			*now = now.Add(6 * time.Second)
			svc.tick()
			st := svc.Store.snapshot()
			if st.Runs[run.ID].Run.Status != tc.want || provider.submits != 1 {
				t.Fatal("failure outcome wrong")
			}
			if tc.stop && (st.Plans[plan.ID].Plan.Enabled || st.Plans[plan.ID].Plan.KeyStatus != "quota_exhausted") {
				t.Fatal("quota failure did not pause plan")
			}
			raw, _ := json.Marshal(st.Runs[run.ID].Run)
			if bytes.Contains(raw, []byte("private-api")) || bytes.Contains(raw, []byte("sk-never")) {
				t.Fatal("raw provider details leaked")
			}
		})
	}
}

func TestIntelligenceScheduleSingleOwnerAndIntervals(t *testing.T) {
	svc, provider, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, true)
	svc.tick()
	state := svc.Store.snapshot()
	if provider.submits != 2 || len(state.Runs) != 2 || state.Plans[plan.ID].Plan.NextCandy.Sub(*now) != 5*time.Minute || state.Plans[plan.ID].Plan.NextPelican.Sub(*now) != 20*time.Minute {
		t.Fatal("initial schedule or intervals wrong")
	}
	*now = now.Add(6 * time.Minute)
	svc.tick()
	if provider.submits != 2 || len(svc.Store.snapshot().Runs) != 2 {
		t.Fatal("overlapping remote tasks must not be resubmitted")
	}
}

func TestIntelligenceRetentionAndRequestTombstone(t *testing.T) {
	svc, _, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	oldID := ""
	err := svc.Store.change(func(st *intelligenceState) error {
		for _, kind := range []string{"candy", "pelican"} {
			for i := 0; i < 65; i++ {
				id := randomID()
				run := IntelligenceRun{ID: id, PlanID: plan.ID, Benchmark: kind, Status: "succeeded", StartedAt: now.Add(-time.Duration(i) * time.Minute)}
				st.Runs[id] = intelligenceRunRecord{Run: run}
				if kind == "candy" && i == 64 {
					oldID = id
					st.Requests[intelligenceRequestHash(plan.ID, kind, "retained-operation")] = intelligenceRequest{RunID: id, At: *now}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.prune(*now); err != nil {
		t.Fatal(err)
	}
	st := svc.Store.snapshot()
	if len(st.Runs) != 80 {
		t.Fatalf("expected 60 candy + 20 pelican, got %d", len(st.Runs))
	}
	if _, ok := st.Runs[oldID]; ok {
		t.Fatal("old record retained")
	}
	if _, err = svc.Enqueue(plan.ID, "candy", "retained-operation", true); !errors.Is(err, ErrConflict) {
		t.Fatal("pruned idempotent request could be executed again")
	}
}

func TestIntelligencePersistenceFailureStopsSubmission(t *testing.T) {
	svc, provider, _, _ := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	run := enqueueIntelligence(t, svc, plan, "candy")
	svc.Store.writer = func(string, any) error { return errors.New("disk full") }
	svc.tick()
	if provider.submits != 0 || svc.Store.snapshot().Runs[run.ID].Run.Status != "queued" {
		t.Fatal("POST happened before durable intent")
	}
	if _, err := svc.Enqueue(plan.ID, "pelican", "another-operation", true); !errors.Is(err, ErrPersistenceUncertain) {
		t.Fatal("poisoned writer did not fail closed")
	}
}

func TestIntelligenceShutdownWaitsForAcceptedID(t *testing.T) {
	svc, provider, _, _ := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	run := enqueueIntelligence(t, svc, plan, "candy")
	provider.entered, provider.release = make(chan struct{}, 1), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	<-provider.entered
	cancel()
	select {
	case <-done:
		t.Fatal("worker stopped before accepted response was recorded")
	default:
	}
	close(provider.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after bounded call finished")
	}
	if svc.Store.snapshot().Runs[run.ID].RemoteID == "" {
		t.Fatal("accepted ID was lost during graceful shutdown")
	}
}

func TestIntelligenceWrongEncryptionKey(t *testing.T) {
	svc, _, owner, _ := intelligenceFixture(t)
	seedIntelligencePlan(t, svc, false)
	dir := filepath.Dir(owner.path)
	_ = owner.Close()
	other, err := OpenStore(dir, bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = OpenIntelligenceStore(other); err == nil {
		t.Fatal("wrong key was accepted")
	}
}

func TestIntelligenceProviderRejectsRedirectAndNoReplay(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/api/v1/tests" {
			w.Header().Set("Location", "/must-not-follow")
			w.WriteHeader(307)
			return
		}
		t.Error("redirect received the monitoring key")
	}))
	defer server.Close()
	provider := &intelligenceHTTPProvider{client: server.Client(), origin: server.URL}
	_, err := provider.Submit(context.Background(), intelligenceSubmission{APIKey: "sk-test"}, "sources-unique-operation")
	_, definitive, _ := intelligenceErrorCode(err)
	if err == nil || definitive || hits != 1 || strings.Contains(err.Error(), "sk-test") {
		t.Fatal("redirect must be an unknown, non-replayed outcome")
	}
}

func TestIntelligenceProviderContractAndNeutralError(t *testing.T) {
	var seen intelligenceSubmission
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			_ = json.NewDecoder(r.Body).Decode(&seen)
			if r.Header.Get("Idempotency-Key") != "sources-operation" {
				t.Error("missing operation ID")
			}
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"task-fixture-123","benchmark":"candy","status":"running","phase":"generating"}`))
		} else {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"insufficient_balance on https://private.example with sk-secret"}`))
		}
	}))
	defer server.Close()
	provider := &intelligenceHTTPProvider{client: server.Client(), origin: server.URL}
	out, err := provider.Submit(context.Background(), intelligenceSubmission{Benchmark: "candy", BaseURL: "https://main.example/v1", APIKey: "sk-test", Model: intelligenceModel, Effort: "medium"}, "sources-operation")
	if err != nil || out.ID == "" || seen.BaseURL != "https://main.example/v1" || seen.Model != intelligenceModel || seen.Effort != "medium" {
		t.Fatal("request contract changed", err)
	}
	_, err = provider.Poll(context.Background(), out.ID)
	code, definitive, _ := intelligenceErrorCode(err)
	if code != "quota_exhausted" || !definitive || strings.Contains(err.Error(), "private") {
		t.Fatal("native budget failure was not classified safely")
	}
}
