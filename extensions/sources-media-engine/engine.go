// Package mediaengine owns durable upstream tasks. Native identity and money remain
// behind Boundary, implemented only by Main; the engine has no balance API.
package mediaengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrConflict          = errors.New("task conflicts with its original submission")
	ErrNotFound          = errors.New("task not found")
	ErrCapacity          = errors.New("task record capacity reached")
	ErrDenied            = errors.New("task admission denied")
	ErrDeferred          = errors.New("provider request not started; try later")
	ErrRejected          = errors.New("provider rejected the request before acceptance")
	ErrNotOwner          = errors.New("task operation requires the engine owner")
	ErrFinancialConflict = errors.New("task money evidence requires review")
)

type Owner struct{ UserID, KeyID, GroupID int64 }
type Input struct {
	Owner                       Owner
	IdempotencyKey, PayloadHash string
	Body, Ticket                json.RawMessage
	Credential                  Credential
}
type Task struct {
	ID                   string          `json:"id"`
	Owner                Owner           `json:"owner"`
	AccountID            int64           `json:"account_id"`
	IdempotencyKey       string          `json:"idempotency_key"`
	PayloadHash          string          `json:"payload_hash"`
	Body                 json.RawMessage `json:"body,omitempty"`
	Ticket               json.RawMessage `json:"ticket,omitempty"`
	Credential           Credential      `json:"credential"`
	Phase                string          `json:"phase"`
	UpstreamID           string          `json:"upstream_id,omitempty"`
	SourceURLs           []string        `json:"source_urls,omitempty"`
	Result               json.RawMessage `json:"result,omitempty"`
	Failure              string          `json:"failure,omitempty"`
	CreatedAt            int64           `json:"created_at"`
	UpdatedAt            int64           `json:"updated_at"`
	ReviewAfterAt        int64           `json:"review_after_at,omitempty"`
	TerminalAt           int64           `json:"terminal_at,omitempty"`
	NextRun              int64           `json:"next_run"`
	AdmissionHold        string          `json:"admission_hold,omitempty"`
	Version              int64           `json:"version"`
	Attempts             int             `json:"attempts"`
	SubmittedAt          int64           `json:"submitted_at,omitempty"`
	RecoveryPolicyID     string          `json:"recovery_policy_id,omitempty"`
	RecoveryLegacyImport bool            `json:"recovery_legacy_import,omitempty"`
	RecoveryDeadlineAt   int64           `json:"recovery_deadline_at,omitempty"`
	RecoveryManualReason string          `json:"recovery_manual_reason,omitempty"`
	SupplierOutcome      string          `json:"supplier_outcome,omitempty"`
	CustomerClosedAt     int64           `json:"customer_closed_at,omitempty"`
}
type Boundary interface {
	Reserve(context.Context, *Task) error
	Release(context.Context, *Task) error
	Settle(context.Context, *Task) error
	StoreResult(context.Context, *Task, []string) (json.RawMessage, error)
}
type Provider interface {
	Submit(context.Context, Credential, json.RawMessage, string) (string, error)
	Poll(context.Context, Credential, string) (Poll, error)
}

// Generator completes one native image request inside the existing durable job.
// It must return only validated results, and ErrDeferred only before submission.
// Older remote tasks still use Provider.Poll; uncertain POSTs are never retried.
type Generator interface {
	Generate(context.Context, *Task) ([]string, error)
}
type Options struct {
	MaxRecordsPerKey, Workers                   int
	PollInterval, OperationTimeout, ReviewAfter time.Duration
	GenerationTimeout                           time.Duration
	Now                                         func() time.Time
	OnError                                     func(string, string)
	RecoveryPolicy                              *RecoveryPolicy
	RecoveryEnabled                             func() bool
}

func defaults(o Options) Options {
	if o.MaxRecordsPerKey <= 0 {
		o.MaxRecordsPerKey = 500000
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Workers > 16 {
		o.Workers = 16
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 3 * time.Second
	}
	if o.OperationTimeout <= 0 {
		o.OperationTimeout = 45 * time.Second
	}
	if o.GenerationTimeout <= 0 {
		o.GenerationTimeout = 30 * time.Minute
	}
	if o.ReviewAfter <= 0 {
		o.ReviewAfter = 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type Engine struct {
	*Producer
	boundary         Boundary
	provider         Provider
	mu               sync.Mutex
	running          map[string]bool
	stopping         bool
	cancel           context.CancelFunc
	cancelOperations context.CancelFunc
	wg               sync.WaitGroup
	steps            sync.WaitGroup
}

func New(s *Store, b Boundary, p Provider) (*Engine, error) {
	return NewWithOptions(s, b, p, Options{})
}
func NewWithOptions(s *Store, b Boundary, p Provider, o Options) (*Engine, error) {
	if s == nil || b == nil || p == nil {
		return nil, errors.New("task engine dependencies required")
	}
	if err := s.requireOwner(); err != nil {
		return nil, err
	}
	if !s.engineClaimed.CompareAndSwap(false, true) {
		return nil, errors.New("task store already attached to an engine")
	}
	producer, err := NewProducer(s, o)
	if err != nil {
		s.engineClaimed.Store(false)
		return nil, err
	}
	e := &Engine{Producer: producer, boundary: b, provider: p, running: map[string]bool{}}
	// Only the holder of the legacy engine lock repairs submit intents. Producers
	// may admit new work concurrently but cannot execute or repair existing tasks.
	after := ""
	for {
		rows, err := s.PhasePage("submitting", after, 100)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, t := range rows {
			after = t.ID
			t.Phase = "unknown"
			t.Failure = "submission_outcome_unknown"
			if err = s.Save(t); err != nil {
				return nil, err
			}
		}
	}
	return e, nil
}
func (e *Engine) Start() {
	if e.store.requireOwner() != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil || e.stopping {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	operations, cancelOperations := context.WithCancel(context.Background())
	e.cancelOperations = cancelOperations
	e.startRecoveryLoops(ctx, operations)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		retention := time.NewTicker(time.Hour)
		defer retention.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.sweep(ctx, operations)
			case <-retention.C:
				if err := e.store.Retain(e.opts.Now()); err != nil && e.opts.OnError != nil {
					e.opts.OnError("", "retention")
				}
			}
		}
	}()
}
func (e *Engine) Close() {
	e.close(false)
}

// CloseGracefully stops scheduling, lets claimed external operations finish
// under their bounded operation/generation timeout, and waits for persistence. It does not turn
// an in-flight Submit into an unknown outcome merely because ownership moves.
// Store.Close must follow this call; the engine lock stays held while draining.
func (e *Engine) CloseGracefully() {
	e.close(true)
}

func (e *Engine) close(graceful bool) {
	e.mu.Lock()
	e.stopping = true
	if e.cancel != nil {
		e.cancel()
	}
	if !graceful && e.cancelOperations != nil {
		e.cancelOperations()
	}
	e.mu.Unlock()
	e.wg.Wait()
	e.steps.Wait()
	if graceful && e.cancelOperations != nil {
		e.cancelOperations()
	}
}
func (e *Engine) Sweep(ctx context.Context) {
	// The explicit one-pass API keeps its synchronous contract. Start runs these
	// bounded lanes independently so slow generation cannot delay money closure.
	e.sweepRecovery(ctx, ctx)
	e.sweepReleases(ctx, ctx)
	e.sweep(ctx, ctx)
}

func (e *Engine) sweep(schedule, operations context.Context) {
	if e.store.requireOwner() != nil {
		return
	}
	rows, err := e.store.Due(e.opts.Now().UnixMilli(), e.opts.Workers*4)
	if err != nil {
		if e.opts.OnError != nil {
			e.opts.OnError("", "due_query")
		}
		return
	}
	e.stepRows(schedule, operations, rows, e.opts.Workers)
}

func (e *Engine) stepRows(schedule, operations context.Context, rows []*Task, workers int) {
	if len(rows) == 0 {
		return
	}
	jobs := make(chan *Task)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if schedule.Err() != nil {
					continue
				}
				if err := e.Step(operations, t.ID); err != nil && e.opts.OnError != nil {
					e.opts.OnError(t.ID, t.Phase)
				}
			}
		}()
	}
	for _, t := range rows {
		if schedule.Err() != nil {
			break
		}
		select {
		case <-schedule.Done():
		case jobs <- t:
		}
	}
	close(jobs)
	wg.Wait()
}
func (e *Engine) claim(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping || e.running[id] {
		return false
	}
	e.running[id] = true
	e.steps.Add(1)
	return true
}
func (e *Engine) unclaim(id string) {
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
	e.steps.Done()
}
func (e *Engine) Step(ctx context.Context, id string) error {
	if err := e.store.requireOwner(); err != nil {
		return err
	}
	if !e.claim(id) {
		return nil
	}
	defer e.unclaim(id)
	t, err := e.store.Get(id)
	if err != nil {
		return err
	}
	// Admission holds are explicitly released by their migration token. Even a
	// direct Step must not bypass the scheduler's far-future sentinel.
	if t.AdmissionHold != "" {
		return ErrDeferred
	}
	timeout := e.opts.OperationTimeout
	generator, native := e.provider.(Generator)
	if native && t.Phase == "queued" {
		timeout = e.opts.GenerationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	save := func(phase string, delay time.Duration) error {
		t.Phase = phase
		t.Attempts = 0
		t.UpdatedAt = e.opts.Now().Unix()
		t.NextRun = e.opts.Now().Add(delay).UnixMilli()
		if phase == "completed" || phase == "failed" {
			t.TerminalAt = t.UpdatedAt
			t.Body = nil
			t.Credential = Credential{}
		}
		if phase == "failed" && t.Failure == automaticUnknownRelease && t.CustomerClosedAt > 0 {
			return e.store.saveWithEvent(t, 0, "auto_release_unknown_completed", fmt.Sprintf("policy=%s; legacy_import=%t; supplier_outcome=unknown", t.RecoveryPolicyID, t.RecoveryLegacyImport))
		}
		return e.store.Save(t)
	}
	retry := func(cause error) error {
		t.Attempts++
		delay := e.opts.PollInterval * time.Duration(1<<min(t.Attempts, 7))
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		t.UpdatedAt = e.opts.Now().Unix()
		t.NextRun = e.opts.Now().Add(delay).UnixMilli()
		return errors.Join(cause, e.store.Save(t))
	}
	if (t.Phase == "running" || t.Phase == "downloading") && e.opts.Now().Unix() >= t.ReviewAfterAt {
		t.Failure = "task_requires_review"
		if t.Phase == "downloading" {
			return save("delivery_review", 0)
		}
		return save("unknown", 0)
	}
	if t.Phase == "queued" && e.opts.Now().Unix() >= t.ReviewAfterAt {
		// queued means no upstream request began (or transport explicitly deferred it).
		// The original hold exists; a broken input store must not freeze money forever.
		t.Failure = "pre_submit_expired"
		return save("releasing", 0)
	}
	switch t.Phase {
	case "admitting":
		if err = e.boundary.Reserve(ctx, t); err != nil {
			if errors.Is(err, ErrDenied) {
				t.Failure = "admission_denied"
				return save("failed", 0)
			}
			return retry(err)
		}
		return save("queued", 0)
	case "submitting":
		// A live Step holds the in-process claim. Reaching this case means its
		// outcome could not be persisted; never turn an uncertain POST into a retry.
		t.Failure = "submission_outcome_unknown"
		return save("unknown", 0)
	case "queued":
		if e.recoveryEnabled() && e.opts.RecoveryPolicy.eligible(t) {
			blocked, checkErr := e.store.recoveryCircuitOpen(t.AccountID, e.opts.Now(), e.opts.RecoveryPolicy)
			if checkErr != nil {
				return retry(checkErr)
			}
			if blocked {
				// No upstream request has started for this queued task. Close this
				// admission rather than holding customer money during the cooldown.
				t.Failure = "recovery_circuit_open"
				return save("releasing", 0)
			}
		}
		if preparer, ok := e.boundary.(interface {
			PrepareSubmit(context.Context, *Task) error
		}); ok {
			if err = preparer.PrepareSubmit(ctx, t); err != nil {
				return retry(err)
			}
		}
		t.SubmittedAt = e.opts.Now().Unix()
		if err = save("submitting", 0); err != nil {
			if errors.Is(err, ErrDeferred) {
				return save("queued", e.opts.PollInterval)
			}
			return err
		}
		var ref string
		var urls []string
		var cause error
		if native {
			urls, cause = generator.Generate(ctx, t)
		} else {
			ref, cause = e.provider.Submit(ctx, t.Credential, t.Body, t.ID)
		}
		if errors.Is(cause, ErrDeferred) {
			if err = save("queued", e.opts.PollInterval); err != nil {
				return err
			}
			return nil
		}
		if errors.Is(cause, ErrRejected) {
			t.Failure = "provider_rejected"
			return save("releasing", 0)
		}
		if cause != nil || (native && len(urls) != 1) || (!native && !refPattern.MatchString(ref)) {
			t.Failure = "submission_outcome_unknown"
			return save("unknown", 0)
		}
		if native {
			// Persist the completed bytes before charging. The existing settling /
			// downloading phases can resume without another generation request.
			t.SourceURLs = urls
			return save("settling", 0)
		}
		t.UpstreamID = ref
		return save("running", e.opts.PollInterval)
	case "running":
		result, cause := e.provider.Poll(ctx, t.Credential, t.UpstreamID)
		if cause != nil {
			return retry(cause)
		}
		if result.Status == "failed" {
			t.Failure = "upstream_failed"
			return save("releasing", 0)
		}
		if result.Status != "completed" {
			return save("running", e.opts.PollInterval)
		}
		if len(result.URLs) != 1 {
			return retry(errors.New("expected one completed image"))
		}
		t.SourceURLs = result.URLs
		return save("settling", 0)
	case "settling":
		// Generation succeeded: settle exactly once even if delivery later fails.
		if err = e.boundary.Settle(ctx, t); err != nil {
			return retry(err)
		}
		return save("downloading", 0)
	case "downloading":
		if t.Attempts > 0 && t.UpstreamID != "" {
			if fresh, cause := e.provider.Poll(ctx, t.Credential, t.UpstreamID); cause == nil && fresh.Status == "completed" && len(fresh.URLs) == 1 {
				t.SourceURLs = fresh.URLs
			}
		}
		result, cause := e.boundary.StoreResult(ctx, t, t.SourceURLs)
		if cause != nil {
			return retry(cause)
		}
		if !json.Valid(result) {
			return retry(errors.New("invalid stored image result"))
		}
		t.Result = result
		t.SourceURLs = nil
		return save("completed", 0)
	case "releasing":
		if err = e.boundary.Release(ctx, t); err != nil {
			if errors.Is(err, ErrFinancialConflict) && t.Failure == automaticUnknownRelease {
				return e.recoveryManual(t, "financial_conflict")
			}
			return retry(err)
		}
		if t.Failure == automaticUnknownRelease {
			t.CustomerClosedAt = e.opts.Now().Unix()
		}
		return save("failed", 0)
	case "unknown":
		return e.recoverUnknown(ctx, t)
	}
	return nil
}

// Resolve is an explicit operator decision, not a provider-submit retry. It can
// bind a confirmed reference, retry only delivery, or release the ORIGINAL hold.
// Operator identity/reason come from Main's authenticated admin route.
func (e *Engine) Resolve(ctx context.Context, id, action, ref string, operator int64, reason string) error {
	if err := e.store.requireOwner(); err != nil {
		return err
	}
	if operator <= 0 || len(strings.TrimSpace(reason)) < 8 || len(reason) > 2000 {
		return ErrConflict
	}
	if !e.claim(id) {
		return ErrConflict
	}
	defer e.unclaim(id)
	ctx, cancel := context.WithTimeout(ctx, e.opts.OperationTimeout)
	defer cancel()
	t, err := e.store.Get(id)
	if err != nil {
		return err
	}
	next := ""
	switch action {
	case "bind":
		if t.Phase != "unknown" || !refPattern.MatchString(ref) || (t.UpstreamID != "" && t.UpstreamID != ref) {
			return ErrConflict
		}
		query, cause := e.provider.Poll(ctx, t.Credential, ref)
		if cause != nil {
			return cause
		}
		if query.Status != "processing" && query.Status != "completed" && query.Status != "failed" {
			return ErrConflict
		}
		t.UpstreamID = ref
		next = "running"
	case "release":
		if t.Phase != "unknown" {
			return ErrConflict
		}
		t.Failure = "operator_released_unknown"
		next = "releasing"
	case "retry_delivery":
		if t.Phase != "delivery_review" {
			return ErrConflict
		}
		next = "downloading"

	default:
		return ErrConflict
	}
	t.Phase = next
	t.RecoveryManualReason = ""
	t.ReviewAfterAt = e.opts.Now().Add(e.opts.ReviewAfter).Unix()
	t.Attempts = 0
	t.NextRun = e.opts.Now().UnixMilli()
	t.UpdatedAt = e.opts.Now().Unix()
	return e.store.saveWithEvent(t, operator, action, fmt.Sprintf("%s: %s", ref, reason))
}
