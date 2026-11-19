package mediaengine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const automaticUnknownRelease = "automatic_unknown_release"
const recoveryReleaseAction = "auto_release_unknown"
const recoveryHistoricalReleaseAction = "auto_release_historical_unknown"

// RecoveryPolicy is an explicit customer-service policy, not proof that the
// supplier failed or refunded a request. Money uses the native 1e-8 precision.
// A nil policy keeps the historical manual-only behavior.
type RecoveryPolicy struct {
	ID                       string   `json:"id"`
	EffectiveFrom            int64    `json:"effective_from_unix"`
	TimeoutSeconds           int64    `json:"timeout_seconds"`
	MaxReleaseAmount         string   `json:"max_release_amount"`
	MaxClosuresPerAccount24h int      `json:"max_closures_per_account_24h"`
	LegacyTaskIDs            []string `json:"legacy_task_ids,omitempty"`
}

var recoveryPolicyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
var recoveryMoney = regexp.MustCompile(`^(0|[1-9][0-9]{0,7})(\.[0-9]{1,8})?$`)

// ParseRecoveryAmount accepts decimal money without rounding or exponent form.
func ParseRecoveryAmount(amount string) (int64, error) {
	if !recoveryMoney.MatchString(amount) {
		return 0, ErrConflict
	}
	parts := strings.SplitN(amount, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrConflict
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 8-len(fraction))
	minor, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, ErrConflict
	}
	return whole*100000000 + minor, nil
}

func (p RecoveryPolicy) Validate() error {
	maxAmount, err := ParseRecoveryAmount(p.MaxReleaseAmount)
	if !recoveryPolicyID.MatchString(p.ID) || p.EffectiveFrom <= 0 || p.TimeoutSeconds < 1800 || p.TimeoutSeconds > 86400 || err != nil || maxAmount <= 0 || p.MaxClosuresPerAccount24h < 1 || p.MaxClosuresPerAccount24h > 1000 || len(p.LegacyTaskIDs) > 1000 {
		return errors.New("invalid bounded media engine recovery policy")
	}
	seen := map[string]bool{}
	for _, id := range p.LegacyTaskIDs {
		// Shape only: the store's namespace decides which ids are its own.
		if !taskIDShape.MatchString(id) || seen[id] {
			return errors.New("invalid media engine historical recovery scope")
		}
		seen[id] = true
	}
	return nil
}

func (p *RecoveryPolicy) eligible(t *Task) bool {
	if p == nil || t == nil {
		return false
	}
	if t.CreatedAt >= p.EffectiveFrom {
		return true
	}
	return p.historical(t)
}

func (p *RecoveryPolicy) historical(t *Task) bool {
	if p == nil || t == nil || t.CreatedAt >= p.EffectiveFrom {
		return false
	}
	for _, id := range p.LegacyTaskIDs {
		if id == t.ID {
			return true
		}
	}
	return false
}

// RecoverySummary contains only operational metadata safe for administrator UI.
type RecoverySummary struct {
	Status               string `json:"recovery_status"`
	DeadlineAt           int64  `json:"recovery_deadline_at,omitempty"`
	ManualActionRequired bool   `json:"manual_action_required"`
}

func (p *Producer) recoveryEnabled() bool {
	return p != nil && p.opts.RecoveryPolicy != nil && p.opts.RecoveryEnabled != nil && p.opts.RecoveryEnabled()
}

func (p *Producer) RecoverySummary(t *Task) RecoverySummary {
	result := RecoverySummary{Status: "disabled", ManualActionRequired: true}
	if t == nil {
		return result
	}
	if t.CustomerClosedAt > 0 && t.SupplierOutcome == "unknown" {
		return RecoverySummary{Status: "closed_unknown", DeadlineAt: t.RecoveryDeadlineAt}
	}
	if t.Failure == automaticUnknownRelease && t.Phase == "releasing" {
		return RecoverySummary{Status: "closing", DeadlineAt: t.RecoveryDeadlineAt}
	}
	if t.RecoveryManualReason != "" || t.Phase == "delivery_review" {
		return RecoverySummary{Status: "manual_required", DeadlineAt: t.RecoveryDeadlineAt, ManualActionRequired: true}
	}
	if !p.recoveryEnabled() {
		return result
	}
	policy := p.opts.RecoveryPolicy
	if !policy.eligible(t) {
		result.Status = "excluded"
		return result
	}
	if t.RecoveryPolicyID != "" && t.RecoveryPolicyID != policy.ID {
		return RecoverySummary{Status: "manual_required", DeadlineAt: t.RecoveryDeadlineAt, ManualActionRequired: true}
	}
	start := t.SubmittedAt
	if start <= 0 {
		start = t.CreatedAt
	}
	deadline := t.RecoveryDeadlineAt
	if deadline <= 0 {
		deadline = start + policy.TimeoutSeconds
	}
	return RecoverySummary{Status: "recovering", DeadlineAt: deadline}
}

// UnknownReleaseAmount is provided by Main after authenticating the original
// immutable money ticket. It returns 1e-8 units; the engine never invents prices.
type unknownReleaseAmount interface{ UnknownReleaseAmount(*Task) (int64, error) }

// These two small lanes share the engine owner, per-task claims, operation
// cancellation and shutdown WaitGroup. Recovery can only query existing work;
// release has its own worker so slow generation or supplier lookups cannot
// occupy all workers needed to finish an already-approved money intent.
func (e *Engine) startRecoveryLoops(schedule, operations context.Context) {
	for _, sweep := range []func(context.Context, context.Context){e.sweepRecovery, e.sweepReleases} {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-schedule.Done():
					return
				case <-ticker.C:
					sweep(schedule, operations)
				}
			}
		}()
	}
}

func (e *Engine) sweepRecovery(schedule, operations context.Context) {
	if e.store.requireOwner() != nil || !e.recoveryEnabled() {
		return
	}
	workers := min(e.opts.Workers, 2)
	rows, err := e.store.recoveryDue(e.opts.Now().UnixMilli(), workers*4, e.opts.RecoveryPolicy)
	if err != nil {
		if e.opts.OnError != nil {
			e.opts.OnError("", "recovery_due_query")
		}
		return
	}
	e.stepRows(schedule, operations, rows, workers)
}

func (e *Engine) sweepReleases(schedule, operations context.Context) {
	if e.store.requireOwner() != nil {
		return
	}
	rows, err := e.store.page("phase='releasing' AND next_run<=? ORDER BY next_run,id LIMIT 16", e.opts.Now().UnixMilli())
	if err != nil {
		if e.opts.OnError != nil {
			e.opts.OnError("", "release_due_query")
		}
		return
	}
	e.stepRows(schedule, operations, rows, 1)
}

func (s *Store) recoveryDue(now int64, limit int, p *RecoveryPolicy) ([]*Task, error) {
	if p == nil {
		return nil, nil
	}
	where := "phase='unknown' AND next_run<=? AND (created_at>=?"
	args := []any{now, p.EffectiveFrom}
	if len(p.LegacyTaskIDs) > 0 {
		where += " OR id IN (" + strings.TrimRight(strings.Repeat("?,", len(p.LegacyTaskIDs)), ",") + ")"
		for _, id := range p.LegacyTaskIDs {
			args = append(args, id)
		}
	}
	where += ") ORDER BY next_run,id LIMIT ?"
	args = append(args, boundedLimit(limit))
	return s.page(where, args...)
}

// Count pending uncertainty as well as recently approved platform-funded
// closures. The threshold stops only new generation, never an accepted release.
// It is a circuit threshold, not a strict global spend cap: an already-running
// batch can finish afterwards and still deserves customer closure.
func (s *Store) recoveryCircuitOpen(account int64, now time.Time, p *RecoveryPolicy) (bool, error) {
	// Explicitly imported historical backlog belongs to its audited remediation
	// batch. It must not consume the new-generation risk budget.
	where := "account_id=? AND phase='unknown'"
	args := []any{account}
	if len(p.LegacyTaskIDs) > 0 {
		where += " AND NOT (created_at<? AND id IN (" + strings.TrimRight(strings.Repeat("?,", len(p.LegacyTaskIDs)), ",") + "))"
		args = append(args, p.EffectiveFrom)
		for _, id := range p.LegacyTaskIDs {
			args = append(args, id)
		}
	}
	args = append(args, account, recoveryReleaseAction, now.Add(-24*time.Hour).Unix())
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM (
 SELECT id FROM tasks WHERE `+where+`
 UNION SELECT e.task_id FROM task_events e JOIN tasks t ON t.id=e.task_id
 WHERE t.account_id=? AND e.action=? AND e.created_at>=?)`, args...).Scan(&count)
	return count >= p.MaxClosuresPerAccount24h, err
}

func (e *Engine) recoverySave(t *Task, phase string, delay time.Duration, action string) error {
	t.Phase = phase
	t.UpdatedAt = e.opts.Now().Unix()
	t.NextRun = e.opts.Now().Add(delay).UnixMilli()
	return e.store.saveWithEvent(t, 0, action, fmt.Sprintf("policy=%s; legacy_import=%t; supplier_outcome=%s; reason=%s", t.RecoveryPolicyID, t.RecoveryLegacyImport, t.SupplierOutcome, t.RecoveryManualReason))
}

func (e *Engine) recoveryManual(t *Task, reason string) error {
	action := "auto_recovery_review_required"
	if t.RecoveryManualReason == reason {
		action = ""
	}
	t.RecoveryManualReason = reason
	return e.recoverySave(t, "unknown", time.Hour, action)
}

func (e *Engine) recoverUnknown(ctx context.Context, t *Task) error {
	policy := e.opts.RecoveryPolicy
	if !e.recoveryEnabled() || !policy.eligible(t) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if t.RecoveryManualReason != "" {
		return e.recoverySave(t, "unknown", time.Hour, "")
	}
	if t.RecoveryPolicyID != "" && t.RecoveryPolicyID != policy.ID {
		return e.recoveryManual(t, "policy_changed")
	}
	if t.RecoveryPolicyID == "" {
		t.RecoveryPolicyID = policy.ID
		t.RecoveryLegacyImport = policy.historical(t)
		start := t.SubmittedAt
		if start <= 0 {
			start = t.CreatedAt
		}
		t.RecoveryDeadlineAt = start + policy.TimeoutSeconds
	}
	// A task carrying durable success evidence must never be automatically released.
	if len(t.SourceURLs) > 0 || len(t.Result) > 0 {
		return e.recoveryManual(t, "success_evidence_present")
	}
	if t.UpstreamID != "" {
		result, err := e.provider.Poll(ctx, t.Credential, t.UpstreamID)
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		if !e.recoveryEnabled() {
			return nil
		}
		if err == nil {
			switch result.Status {
			case "completed":
				if len(result.URLs) != 1 {
					return e.recoveryManual(t, "invalid_success_evidence")
				}
				t.SourceURLs = result.URLs
				t.SupplierOutcome = "success"
				t.Failure = ""
				t.Attempts = 0
				t.ReviewAfterAt = e.opts.Now().Add(e.opts.ReviewAfter).Unix()
				return e.recoverySave(t, "settling", 0, "auto_recovered_success")
			case "failed":
				t.SupplierOutcome = "failed"
				t.Failure = "upstream_failed"
				t.Attempts = 0
				return e.recoverySave(t, "releasing", 0, "auto_recovered_failure")
			}
		}
	}
	if e.opts.Now().Unix() < t.RecoveryDeadlineAt {
		t.Attempts++
		delay := time.Minute * time.Duration(1<<min(t.Attempts-1, 2))
		return e.recoverySave(t, "unknown", delay, "")
	}
	amountBoundary, ok := e.boundary.(unknownReleaseAmount)
	if !ok {
		return e.recoveryManual(t, "money_evidence_unavailable")
	}
	amount, err := amountBoundary.UnknownReleaseAmount(t)
	maximum, parseErr := ParseRecoveryAmount(policy.MaxReleaseAmount)
	if err != nil || parseErr != nil || amount < 0 {
		return e.recoveryManual(t, "money_evidence_invalid")
	}
	if amount > maximum {
		return e.recoveryManual(t, "amount_exceeds_policy")
	}
	if !e.recoveryEnabled() {
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	t.SupplierOutcome = "unknown"
	t.Failure = automaticUnknownRelease
	t.Attempts = 0
	// Persist authorization before touching the native ledger. Any crash retries
	// the same release, whose original hold/capture checks remain authoritative.
	action := recoveryReleaseAction
	if t.RecoveryLegacyImport {
		action = recoveryHistoricalReleaseAction
	}
	return e.recoverySave(t, "releasing", 0, action)
}
