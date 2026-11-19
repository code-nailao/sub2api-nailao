package mediaengine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recoveryBoundary struct {
	*boundary
	amount       int64
	amountErr    error
	releaseErr   error
	releaseCalls int
}

func (b *recoveryBoundary) UnknownReleaseAmount(*Task) (int64, error) { return b.amount, b.amountErr }
func (b *recoveryBoundary) Release(ctx context.Context, t *Task) error {
	b.releaseCalls++
	if b.releaseErr != nil {
		return b.releaseErr
	}
	return b.boundary.Release(ctx, t)
}

type recoveryProvider struct {
	provider
	polls   int
	pollErr error
}

func (p *recoveryProvider) Poll(ctx context.Context, c Credential, ref string) (Poll, error) {
	p.polls++
	if p.pollErr != nil {
		return Poll{}, p.pollErr
	}
	return p.provider.Poll(ctx, c, ref)
}

func recoverySetup(t *testing.T) (*Engine, *Store, *recoveryBoundary, *recoveryProvider, *time.Time) {
	t.Helper()
	old, s, b, _, _, _ := setup(t)
	old.Close()
	s.engineClaimed.Store(false)
	now := time.Now().Truncate(time.Second)
	p := &recoveryProvider{provider: provider{status: "completed"}}
	bb := &recoveryBoundary{boundary: b, amount: 3000000}
	policy := &RecoveryPolicy{ID: "test-v1", EffectiveFrom: now.Unix() - 1, TimeoutSeconds: 3600, MaxReleaseAmount: "0.10", MaxClosuresPerAccount24h: 3}
	e, err := NewWithOptions(s, bb, p, Options{Now: func() time.Time { return now }, RecoveryPolicy: policy, RecoveryEnabled: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e, s, bb, p, &now
}

func unknownTask(t *testing.T, e *Engine, p *recoveryProvider) *Task {
	t.Helper()
	p.submitErr = true
	task := create(t, e)
	steps(t, e, task.ID, 2)
	got, err := e.store.Get(task.ID)
	if err != nil || got.Phase != "unknown" {
		t.Fatalf("unknown setup: %v %v", got, err)
	}
	return got
}

func TestRecoveryUnknownDeadlineClosesCustomerWithoutRepostAndRetainsEvidence(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	task := unknownTask(t, e, p)
	steps(t, e, task.ID, 1)
	got, _ := s.Get(task.ID)
	if got.Phase != "unknown" || len(b.released) != 0 || p.posts != 1 || p.polls != 0 {
		t.Fatal("premature closure or request replay")
	}
	if got.RecoveryDeadlineAt != got.SubmittedAt+3600 {
		t.Fatal("deadline not based on submit intent")
	}
	*now = now.Add(61 * time.Minute)
	steps(t, e, task.ID, 2)
	got, _ = s.Get(task.ID)
	if got.Phase != "failed" || !b.released[task.ID] || len(b.settled) != 0 || got.SupplierOutcome != "unknown" || got.CustomerClosedAt != now.Unix() {
		t.Fatalf("bad closure: %+v", got)
	}
	if len(got.Body) != 0 || got.Credential.APIKey != "" || len(got.Ticket) == 0 {
		t.Fatal("closure retained payload/credential or lost money evidence")
	}
	steps(t, e, task.ID, 3)
	if p.posts != 1 || p.polls != 0 || len(b.released) != 1 {
		t.Fatal("terminal closure replayed")
	}
	var events, operator int
	if err := s.db.QueryRow("SELECT count(*),max(operator_id) FROM task_events WHERE task_id=? AND action=?", task.ID, recoveryReleaseAction).Scan(&events, &operator); err != nil {
		t.Fatal(err)
	}
	if events != 1 || operator != 0 {
		t.Fatal("automatic audit is not unique/system owned")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM task_events WHERE task_id=? AND operator_id=0 AND action='auto_release_unknown_completed'", task.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("missing or duplicate committed-release audit", err)
	}
	if err := s.Retain(now.Add(40 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(task.ID)
	if err != nil || got.Phase != "failed" || len(got.Ticket) == 0 || got.SupplierOutcome != "unknown" {
		t.Fatal("retention erased unresolved supplier cost")
	}
	if summary := e.RecoverySummary(got); summary.Status != "closed_unknown" || summary.ManualActionRequired {
		t.Fatal(summary)
	}
}

func TestRecoveryKnownReferencePollsAndResumesSuccessfulDelivery(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	task := create(t, e)
	steps(t, e, task.ID, 2)
	task, _ = s.Get(task.ID)
	task.Phase = "unknown"
	task.ReviewAfterAt = now.Add(-time.Hour).Unix()
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 3)
	got, _ := s.Get(task.ID)
	if got.Phase != "completed" || !b.settled[task.ID] || b.released[task.ID] || p.posts != 1 || p.polls != 1 {
		t.Fatalf("recovery did not use existing result: phase=%s posts=%d polls=%d", got.Phase, p.posts, p.polls)
	}
	if got.SupplierOutcome != "success" {
		t.Fatal("confirmed outcome not recorded")
	}
}

func TestRecoveryKnownFailureReleasesOriginalHold(t *testing.T) {
	e, s, b, p, _ := recoverySetup(t)
	task := create(t, e)
	steps(t, e, task.ID, 2)
	task, _ = s.Get(task.ID)
	task.Phase = "unknown"
	if err := s.Save(task); err != nil {
		t.Fatal(err)
	}
	p.status = "failed"
	steps(t, e, task.ID, 2)
	got, _ := s.Get(task.ID)
	if got.Phase != "failed" || got.SupplierOutcome != "failed" || !b.released[task.ID] || p.posts != 1 || p.polls != 1 {
		t.Fatal("failed original task did not release")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM task_events WHERE action=?", recoveryReleaseAction).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("confirmed failure counted as platform-funded uncertainty")
	}
}

func TestRecoveryProcessingOrLookupFailureClosesOnlyAfterDeadline(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "processing", true: "query_error"}[failed], func(t *testing.T) {
			e, s, b, p, now := recoverySetup(t)
			task := create(t, e)
			steps(t, e, task.ID, 2)
			task, _ = s.Get(task.ID)
			task.Phase = "unknown"
			if err := s.Save(task); err != nil {
				t.Fatal(err)
			}
			p.status = "processing"
			if failed {
				p.pollErr = errors.New("network unavailable")
			}
			steps(t, e, task.ID, 1)
			if len(b.released) != 0 {
				t.Fatal("released before policy deadline")
			}
			*now = now.Add(time.Hour)
			steps(t, e, task.ID, 2)
			got, _ := s.Get(task.ID)
			if got.Phase != "failed" || got.SupplierOutcome != "unknown" || len(b.released) != 1 || p.posts != 1 {
				t.Fatal("uncertain supplier outcome was not retained")
			}
		})
	}
}

func TestRecoveryScopeDisabledAndHistoricalAllowlist(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	task := unknownTask(t, e, p)
	e.opts.RecoveryPolicy.EffectiveFrom = now.Unix() + 1
	*now = now.Add(25 * time.Hour)
	e.Sweep(context.Background())
	if len(b.released) != 0 || e.RecoverySummary(task).Status != "excluded" {
		t.Fatal("historical task escaped policy scope")
	}
	e.opts.RecoveryPolicy.LegacyTaskIDs = []string{task.ID}
	e.Sweep(context.Background())
	e.Sweep(context.Background())
	got, _ := s.Get(task.ID)
	if got.Phase != "failed" || len(b.released) != 1 {
		t.Fatal("allowlisted historical task did not close")
	}
	e.opts.RecoveryPolicy = nil
	if e.RecoverySummary(&Task{Phase: "unknown"}).Status != "disabled" {
		t.Fatal("disabled policy falsely advertises automatic recovery")
	}
}

func TestRecoveryMoneyAndEvidenceGuardsRequireManualReview(t *testing.T) {
	for _, name := range []string{"amount", "negative_amount", "ticket", "success_evidence", "result_evidence", "policy_changed", "financial_conflict"} {
		t.Run(name, func(t *testing.T) {
			e, s, b, p, now := recoverySetup(t)
			task := unknownTask(t, e, p)
			*now = now.Add(time.Hour)
			switch name {
			case "amount":
				b.amount = 11000000
			case "negative_amount":
				b.amount = -1
			case "ticket":
				b.amountErr = ErrConflict
			case "success_evidence":
				task.SourceURLs = []string{"https://owned.example/result"}
			case "result_evidence":
				task.Result = []byte(`{"data":[{"url":"https://owned.example/result"}]}`)
			case "policy_changed":
				task.RecoveryPolicyID = "previous-policy"
			case "financial_conflict":
				b.releaseErr = ErrFinancialConflict
			}
			if name == "success_evidence" || name == "result_evidence" || name == "policy_changed" {
				if err := s.Save(task); err != nil {
					t.Fatal(err)
				}
			}
			steps(t, e, task.ID, 2)
			got, _ := s.Get(task.ID)
			if got.Phase != "unknown" || got.RecoveryManualReason == "" || len(b.released) != 0 || len(b.settled) != 0 || p.posts != 1 {
				t.Fatal("unsafe evidence was finalized")
			}
			if summary := e.RecoverySummary(got); !summary.ManualActionRequired || summary.Status != "manual_required" {
				t.Fatal(summary)
			}
			calls := b.releaseCalls
			steps(t, e, task.ID, 3)
			if b.releaseCalls != calls {
				t.Fatal("review-required financial evidence retried automatically")
			}
		})
	}
}

func TestRecoveryReleaseIntentSurvivesRestartAndTransientFailure(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	task := unknownTask(t, e, p)
	*now = now.Add(time.Hour)
	steps(t, e, task.ID, 1)
	b.releaseErr = errors.New("temporary ledger outage")
	if e.Step(context.Background(), task.ID) == nil {
		t.Fatal("lost ledger failure")
	}
	got, _ := s.Get(task.ID)
	if got.Phase != "releasing" || got.CustomerClosedAt != 0 {
		t.Fatal("claimed customer closure before ledger committed")
	}
	e.Close()
	dir := filepath.Dir(s.lock.Name())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, []byte(strings.Repeat("a", 32)), Namespace{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Removing configuration cannot undo a durably approved release intent.
	resumed, err := NewWithOptions(reopened, b, p, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	b.releaseErr = nil
	steps(t, resumed, task.ID, 2)
	got, _ = reopened.Get(task.ID)
	if got.Phase != "failed" || got.CustomerClosedAt == 0 || p.posts != 1 || len(b.released) != 1 {
		t.Fatal("release intent was lost or replayed generation")
	}
}

func TestRecoveryHistoricalBatchDoesNotConsumeNewGenerationCircuit(t *testing.T) {
	e, s, _, p, now := recoverySetup(t)
	e.opts.RecoveryPolicy.MaxClosuresPerAccount24h = 1
	legacy := unknownTask(t, e, p)
	e.opts.RecoveryPolicy.EffectiveFrom = now.Unix() + 1
	e.opts.RecoveryPolicy.LegacyTaskIDs = []string{legacy.ID}
	*now = now.Add(time.Hour)
	open, err := s.recoveryCircuitOpen(legacy.AccountID, *now, e.opts.RecoveryPolicy)
	if err != nil || open {
		t.Fatal("explicit historical backlog consumed live risk budget", err)
	}
	steps(t, e, legacy.ID, 2)
	closed, _ := s.Get(legacy.ID)
	if closed.Phase != "failed" || !closed.RecoveryLegacyImport {
		t.Fatal("historical responsibility was not retained")
	}
	var historyEvents int
	if err := s.db.QueryRow("SELECT count(*) FROM task_events WHERE task_id=? AND action=? AND operator_id=0", legacy.ID, recoveryHistoricalReleaseAction).Scan(&historyEvents); err != nil || historyEvents != 1 {
		t.Fatal("historical release missing separate audit", err)
	}
	// A later policy no longer needs to carry the import list for these receipts.
	e.opts.RecoveryPolicy.LegacyTaskIDs = nil
	open, err = s.recoveryCircuitOpen(legacy.AccountID, *now, e.opts.RecoveryPolicy)
	if err != nil || open {
		t.Fatal("historical closure counted against subsequent generation", err)
	}
	in := input()
	in.IdempotencyKey = "after-historical-closure"
	task, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, task.ID, 2)
	if p.posts != 2 {
		t.Fatal("historical remediation blocked future generation")
	}
	open, err = s.recoveryCircuitOpen(task.AccountID, *now, e.opts.RecoveryPolicy)
	if err != nil || !open {
		t.Fatal("new unknown task escaped live circuit", err)
	}
	if err := s.Retain(now.Add(40 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	closed, err = s.Get(legacy.ID)
	if err != nil || len(closed.Ticket) == 0 {
		t.Fatal("historical supplier uncertainty lost its original receipt", err)
	}
}

func TestRecoveryDeadlineStartsAtSubmitAndDisabledPolicyKeepsUnknown(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	p.submitErr = true
	task := create(t, e)
	steps(t, e, task.ID, 1)
	*now = now.Add(2 * time.Hour)
	steps(t, e, task.ID, 2)
	got, _ := s.Get(task.ID)
	if got.RecoveryDeadlineAt != now.Add(time.Hour).Unix() || got.Phase != "unknown" || len(b.released) != 0 {
		t.Fatal("queue waiting consumed the post-submit recovery budget")
	}
	e.opts.RecoveryPolicy = nil
	*now = now.Add(48 * time.Hour)
	e.Sweep(context.Background())
	steps(t, e, task.ID, 2)
	got, _ = s.Get(task.ID)
	if got.Phase != "unknown" || b.releaseCalls != 0 || p.posts != 1 {
		t.Fatal("disabled policy changed an unknown task")
	}
}

func TestRecoveryEnableGateStopsNewDecisionsButCompletesApprovedRelease(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	first := unknownTask(t, e, p)
	in := input()
	in.IdempotencyKey = "second-unknown"
	second, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, second.ID, 2)
	first, _ = s.Get(first.ID)
	second, _ = s.Get(second.ID)
	*now = now.Add(time.Hour)
	// No callback is disabled even when a valid policy has been loaded.
	e.opts.RecoveryEnabled = nil
	e.Sweep(context.Background())
	steps(t, e, first.ID, 1)
	unchanged, _ := s.Get(first.ID)
	if unchanged.Version != first.Version || b.releaseCalls != 0 || e.RecoverySummary(unchanged).Status != "disabled" {
		t.Fatal("absent enable gate modified an uncertain task")
	}
	enabled := false
	e.opts.RecoveryEnabled = func() bool { return enabled }
	e.Sweep(context.Background())
	unchanged, _ = s.Get(second.ID)
	if unchanged.Version != second.Version || b.releaseCalls != 0 {
		t.Fatal("closed enable gate modified an uncertain task")
	}
	enabled = true
	steps(t, e, first.ID, 1)
	intent, _ := s.Get(first.ID)
	if intent.Phase != "releasing" || intent.Failure != automaticUnknownRelease || b.releaseCalls != 0 {
		t.Fatal("enable did not persist the bounded release intent first")
	}
	enabled = false
	e.Sweep(context.Background())
	closed, _ := s.Get(first.ID)
	unchanged, _ = s.Get(second.ID)
	if closed.Phase != "failed" || !b.released[first.ID] || unchanged.Version != second.Version || b.releaseCalls != 1 || p.posts != 2 {
		t.Fatal("gate removal lost approved money work or started another decision")
	}
}

func TestRecoveryCircuitOnlyRejectsNewGenerationForAffectedAccount(t *testing.T) {
	e, s, b, p, now := recoverySetup(t)
	e.opts.RecoveryPolicy.MaxClosuresPerAccount24h = 1
	first := unknownTask(t, e, p)
	in := input()
	in.IdempotencyKey = "next-one"
	next, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, next.ID, 3)
	got, _ := s.Get(next.ID)
	if got.Phase != "failed" || got.Failure != "recovery_circuit_open" || !b.released[next.ID] || p.posts != 1 {
		t.Fatal("circuit sent another paid request or froze queued customer")
	}
	in.IdempotencyKey = "other-account"
	in.Credential.AccountID = 2
	other, err := e.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	steps(t, e, other.ID, 2)
	if p.posts != 2 {
		t.Fatal("circuit disabled unrelated account")
	}
	*now = now.Add(time.Hour)
	steps(t, e, first.ID, 2)
	if !b.released[first.ID] {
		t.Fatal("risk circuit blocked an already accepted closure")
	}
}

func TestRecoveryPolicyValidationAndDecimalAmounts(t *testing.T) {
	for _, text := range []string{"0", "0.00000001", "0.06", "100.12345678"} {
		if _, err := ParseRecoveryAmount(text); err != nil {
			t.Fatal(text, err)
		}
	}
	for _, text := range []string{"-1", "1e2", "NaN", "0.000000001", "1.", "01", " 1", "100000000"} {
		if _, err := ParseRecoveryAmount(text); err == nil {
			t.Fatal("accepted", text)
		}
	}
	p := RecoveryPolicy{ID: "v1", EffectiveFrom: 1, TimeoutSeconds: 3600, MaxReleaseAmount: "0.10", MaxClosuresPerAccount24h: 3}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.LegacyTaskIDs = []string{"mediatask_" + strings.Repeat("a", 32), "mediatask_" + strings.Repeat("a", 32)}
	if p.Validate() == nil {
		t.Fatal("duplicate historical scope accepted")
	}
	p.LegacyTaskIDs = nil
	p.TimeoutSeconds = 1
	if p.Validate() == nil {
		t.Fatal("unsafe short timeout accepted")
	}
}
