package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
)

const rejectionRecoveryInterval = 30 * time.Second
const rejectionRecoveryTaskTimeout = 15 * time.Second
const rejectionRecoveryPageSize = 32
const rejectionUnsentReservationMaxAge = 5 * time.Minute

// A backlog of due holds is drained over several runs so it cannot delay the
// task reconciliation that follows in the same run by more than this many
// short transactions.
const rejectionDeadlineReleaseMaxPerRun = 64
const rejectionDeadlineReleaseTimeout = 5 * time.Second

// SettingKeyUpstreamRejectionDeadlineReleasePaused pauses only the deadline
// waiver. "true" keeps every due hold frozen until an operator has reviewed the
// ledger; a missing key or "false" runs the waiver; any other value, including
// an empty one, pauses. Requests, rejection charges, ordinary bills, the
// recovery of unsent reservations and task settlement are not affected. Read
// before every release, so setting it stops a run that is already under way.
const SettingKeyUpstreamRejectionDeadlineReleasePaused = "upstream_rejection_deadline_release_paused"

type rejectionSettingReader interface {
	GetValue(ctx context.Context, key string) (string, error)
}

type rejectionDeadlineReleaser interface {
	UpstreamRejectionDueSource
	FinalizeUpstreamRejection(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error)
}

type rejectionProjectionSource interface {
	ListUnprojectedUpstreamRejectionsPage(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error)
}

type rejectionPendingTaskSource interface {
	ListPendingUpstreamRejectionsPage(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error)
}

// UpstreamRejectionRecovery observes previously submitted video tasks and repairs
// usage projections from already-settled rejection events without charging again.
// The process-role owner lock is acquired before this provider can start. A
// serving instance never scans, and shutdown joins its one in-flight GET before
// closing the billing, auth-cache, or database dependencies.
type UpstreamRejectionRecovery struct {
	repo             rejectionPendingTaskSource
	resolveKey       func(context.Context, *UpstreamRejectionReservation) (*APIKey, error)
	reconcile        func(context.Context, *UpstreamRejectionReservation, *APIKey) (bool, error)
	projectionRepo   rejectionProjectionSource
	project          func(context.Context, *UpstreamRejectionReservation) error
	projectionCursor *UpstreamRejectionPendingCursor
	releaseUnsent    func(context.Context, *UpstreamRejectionReservation) error
	staleCursor      *UpstreamRejectionPendingCursor
	dueRepo          rejectionDeadlineReleaser
	invalidateDue    func(context.Context, *UpstreamRejectionReservation)
	dueCursor        *UpstreamRejectionDueCursor
	settings         rejectionSettingReader
	// releasePaused is the last pause state that was read, kept only to log a
	// change once. Touched by the scan goroutine alone.
	releasePaused bool

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
	scanMu  sync.Mutex
	cursor  *UpstreamRejectionPendingCursor
}

func ProvideUpstreamRejectionRecovery(gateway *OpenAIGatewayService, billing UsageBillingRepository, keys APIKeyRepository, users UserRepository, updater *APIKeyService) *UpstreamRejectionRecovery {
	repo, _ := billing.(UpstreamRejectionBillingRepository)
	w := &UpstreamRejectionRecovery{repo: repo, projectionRepo: repo}
	if gateway != nil {
		w.project = gateway.ProjectUpstreamRejectionUsage
	}
	if gateway != nil && keys != nil && users != nil {
		w.resolveKey = func(ctx context.Context, r *UpstreamRejectionReservation) (*APIKey, error) {
			key, err := keys.GetByID(ctx, r.APIKeyID)
			if errors.Is(err, ErrAPIKeyNotFound) {
				// A tombstoned key cannot authenticate new calls, but its already
				// submitted task still owns a durable financial obligation.
				key = &APIKey{ID: r.APIKeyID, UserID: r.UserID}
			} else if err != nil {
				return nil, err
			}
			if key == nil || key.UserID != r.UserID {
				return nil, ErrUpstreamRejectionConflict
			}
			user, err := users.GetByID(ctx, r.UserID)
			if err != nil {
				return nil, err
			}
			if user == nil || user.ID != r.UserID {
				return nil, ErrUpstreamRejectionConflict
			}
			copyKey := *key
			copyKey.User = user
			return RestoreGrokVideoRejectionBillingKey(&copyKey, r)
		}
		w.reconcile = func(ctx context.Context, r *UpstreamRejectionReservation, key *APIKey) (bool, error) {
			return gateway.ReconcileGrokVideoRejection(ctx, r, key, updater)
		}
	}
	if repo != nil && gateway != nil {
		w.releaseUnsent = func(ctx context.Context, r *UpstreamRejectionReservation) error {
			result, err := repo.FinalizeUpstreamRejection(ctx, &UpstreamRejectionFinalizeCommand{
				RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionReleased,
				// Intentionally false: a concurrent MarkSent wins safely and
				// makes this stale snapshot ineligible for release.
				ConfirmedNoCharge: false,
			})
			if err == nil && result != nil && result.Applied {
				gateway.invalidateUpstreamRejectionCaches(ctx, &APIKey{ID: r.APIKeyID, UserID: r.UserID}, updater)
			}
			return err
		}
	}
	if due, ok := billing.(rejectionDeadlineReleaser); ok && gateway != nil {
		w.dueRepo = due
		w.invalidateDue = func(ctx context.Context, r *UpstreamRejectionReservation) {
			gateway.invalidateUpstreamRejectionCaches(ctx, &APIKey{ID: r.APIKeyID, UserID: r.UserID}, updater)
		}
	}
	w.settings = rejectionPauseSettingReader(gateway)
	processrole.StartBackground("UpstreamRejectionRecovery", w.Start)
	return w
}

func (w *UpstreamRejectionRecovery) Start() {
	if w == nil || !processrole.BackgroundEnabled() || w.repo == nil || w.resolveKey == nil || w.reconcile == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil || w.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(rejectionRecoveryInterval)
		defer ticker.Stop()
		for {
			w.runOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *UpstreamRejectionRecovery) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (w *UpstreamRejectionRecovery) runOnce(ctx context.Context) {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	w.recoverUsageProjections(ctx)
	w.recoverUnsentReservations(ctx)
	w.releaseDueReservations(ctx)
	if ctx.Err() != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	page, err := w.repo.ListPendingUpstreamRejectionsPage(readCtx, &UpstreamRejectionPendingQuery{Limit: rejectionRecoveryPageSize, After: w.cursor, TasksOnly: true})
	cancel()
	if err != nil || page == nil {
		logger.LegacyPrintf("service.rejection_recovery", "pending task scan failed; no billing transitions applied")
		return
	}
	// Advance even for an unresolvable task so it cannot starve later tasks.
	w.cursor = page.Next
	for i := range page.Items {
		if ctx.Err() != nil {
			return
		}
		r := &page.Items[i]
		if r.Task == nil || (r.State != UpstreamRejectionSent && r.State != UpstreamRejectionUnknown) {
			continue
		}
		taskCtx, cancel := context.WithTimeout(ctx, rejectionRecoveryTaskTimeout)
		key, err := w.resolveKey(taskCtx, r)
		if err == nil {
			_, err = w.reconcile(taskCtx, r, key)
		}
		cancel()
		if err != nil {
			// Do not expose upstream URLs, request bodies, or credentials in logs.
			logger.LegacyPrintf("service.rejection_recovery", "task remains pending: request_id=%s account_id=%d", r.RequestID, r.AccountID)
		}
		// Serial bounded GETs, including when a large recovered page is ready.
		if i+1 < len(page.Items) {
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// recoverUsageProjections consumes the ledger outbox, not the funds state
// machine. The projector must mark the row only after an idempotent usage write.
func (w *UpstreamRejectionRecovery) recoverUsageProjections(ctx context.Context) {
	if w.projectionRepo == nil || w.project == nil || ctx.Err() != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	page, err := w.projectionRepo.ListUnprojectedUpstreamRejectionsPage(readCtx, &UpstreamRejectionPendingQuery{Limit: rejectionRecoveryPageSize, After: w.projectionCursor})
	cancel()
	if err != nil || page == nil {
		logger.LegacyPrintf("service.rejection_recovery", "usage projection scan failed; settled ledger retained")
		return
	}
	w.projectionCursor = page.Next
	for i := range page.Items {
		if ctx.Err() != nil {
			return
		}
		r := &page.Items[i]
		if r.State != UpstreamRejectionRejected {
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := w.project(writeCtx, r)
		cancel()
		if err != nil {
			logger.LegacyPrintf("service.rejection_recovery", "usage projection pending: request_id=%s", r.RequestID)
		}
	}
}

// Only an unsent, stale reservation can be released without supplier evidence.
// Sent/unknown operations remain durable for explicit reconciliation. The PG
// transition rechecks state under lock, so racing MarkSent cannot lose its hold.
func (w *UpstreamRejectionRecovery) recoverUnsentReservations(ctx context.Context) {
	if w.releaseUnsent == nil || w.repo == nil || ctx.Err() != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	page, err := w.repo.ListPendingUpstreamRejectionsPage(readCtx, &UpstreamRejectionPendingQuery{Limit: rejectionRecoveryPageSize, After: w.staleCursor})
	cancel()
	if err != nil || page == nil {
		return
	}
	w.staleCursor = page.Next
	cutoff := time.Now().Add(-rejectionUnsentReservationMaxAge)
	for i := range page.Items {
		if ctx.Err() != nil {
			return
		}
		r := &page.Items[i]
		if r.CreatedAt.IsZero() || !r.CreatedAt.Before(cutoff) {
			continue
		}
		if r.State != UpstreamRejectionReserved || r.SentAt != nil {
			// Rows with a deadline are released by releaseDueReservations.
			if r.Task == nil && r.ReleaseAfter == nil {
				logger.LegacyPrintf("service.rejection_recovery", "sent request awaiting reconciliation; funds retained: request_id=%s", r.RequestID)
			}
			continue
		}
		releaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := w.releaseUnsent(releaseCtx, r)
		cancel()
		if err != nil && !errors.Is(err, ErrUpstreamRejectionState) {
			logger.LegacyPrintf("service.rejection_recovery", "unsent reservation release pending: request_id=%s", r.RequestID)
		}
	}
}

// releaseDueReservations applies the deadline waiver to synchronous holds whose
// outcome stayed unknown past their durable deadline: the site releases the
// hold and absorbs any upstream cost. Each release is its own short
// transaction that rechecks state, task and deadline under the row lock, so a
// racing rejection or normal bill wins safely and counts here as a lost race.
// Rows without a deadline are never listed.
func (w *UpstreamRejectionRecovery) releaseDueReservations(ctx context.Context) {
	if w.dueRepo == nil || ctx.Err() != nil || w.deadlineReleasePaused(ctx) {
		return
	}
	listed, released, lost, failed := 0, 0, 0, 0
	amount := 0.0
	// How long the oldest hold of this run waited past its deadline. The process
	// clock is used for this log figure only; every decision uses the database
	// clock.
	maxOverdue := time.Duration(0)
	defer func() {
		// One line per run with work; never payloads, URLs or credentials.
		if listed > 0 {
			logger.LegacyPrintf("service.rejection_recovery", "deadline release: listed=%d released=%d amount=%.8f lost_races=%d failed=%d max_overdue_s=%.0f", listed, released, amount, lost, failed, maxOverdue.Seconds())
		}
	}()
	for listed < rejectionDeadlineReleaseMaxPerRun && ctx.Err() == nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		page, err := w.dueRepo.ListDueUpstreamRejectionsPage(readCtx, &UpstreamRejectionDueQuery{
			Limit: min(rejectionRecoveryPageSize, rejectionDeadlineReleaseMaxPerRun-listed), After: w.dueCursor,
		})
		cancel()
		if err != nil || page == nil {
			logger.LegacyPrintf("service.rejection_recovery", "deadline release scan failed; holds retained")
			return
		}
		for i := range page.Items {
			if ctx.Err() != nil {
				return
			}
			// Read again before every release: once the operator pauses, not
			// one more hold is released, even in the middle of a backlog. The
			// cursor stays, so the page is resumed after the pause.
			if listed > 0 && w.deadlineReleasePaused(ctx) {
				return
			}
			listed++
			r := &page.Items[i]
			if r.ReleaseAfter != nil {
				if overdue := time.Since(*r.ReleaseAfter); overdue > maxOverdue {
					maxOverdue = overdue
				}
			}
			releaseCtx, cancel := context.WithTimeout(ctx, rejectionDeadlineReleaseTimeout)
			result, err := w.dueRepo.FinalizeUpstreamRejection(releaseCtx, NewUpstreamRejectionDeadlineReleaseCommand(r.RequestID, r.APIKeyID))
			cancel()
			switch {
			case err == nil && result != nil && result.Applied:
				released++
				amount += r.Price
				if w.invalidateDue != nil {
					w.invalidateDue(ctx, r)
				}
			case err == nil, errors.Is(err, ErrUpstreamRejectionState):
				// Already settled by another path (or by an earlier replay).
				lost++
			default:
				failed++
			}
		}
		// Advance only after the whole page was processed, so a cancellation
		// mid-page resumes this page instead of skipping it until wrap-around.
		// Rows that failed are passed so they cannot starve later due rows; a nil
		// cursor restarts from the oldest deadline on the next run.
		w.dueCursor = page.Next
		if page.Next == nil {
			return
		}
	}
}

// rejectionPauseSettingReader is the settings store behind the pause switch,
// or nil when the gateway has none; a nil reader never pauses.
func rejectionPauseSettingReader(gateway *OpenAIGatewayService) rejectionSettingReader {
	if gateway == nil || gateway.settingService == nil || gateway.settingService.settingRepo == nil {
		return nil
	}
	return gateway.settingService.settingRepo
}

// deadlineReleasePaused reads the operator's pause switch. Only a clear "not
// paused" (no key, or "false") lets the waiver run: a retained hold can still
// be released later, a release cannot be taken back. An unreadable switch stops
// this run without changing the remembered state, so a database blip is not
// logged as a pause.
func (w *UpstreamRejectionRecovery) deadlineReleasePaused(ctx context.Context) bool {
	if w.settings == nil {
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	raw, err := w.settings.GetValue(readCtx, SettingKeyUpstreamRejectionDeadlineReleasePaused)
	cancel()
	var paused bool
	switch {
	case errors.Is(err, ErrSettingNotFound):
		paused = false
	case err != nil:
		logger.LegacyPrintf("service.rejection_recovery", "deadline release skipped: pause switch unreadable; holds retained")
		return true
	default:
		paused = strings.TrimSpace(raw) != "false"
	}
	if paused != w.releasePaused {
		w.releasePaused = paused
		if paused {
			logger.LegacyPrintf("service.rejection_recovery", "deadline release paused by setting %s; due holds retained", SettingKeyUpstreamRejectionDeadlineReleasePaused)
		} else {
			logger.LegacyPrintf("service.rejection_recovery", "deadline release resumed")
		}
	}
	return paused
}
