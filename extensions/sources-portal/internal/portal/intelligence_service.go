package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

type IntelligenceService struct {
	Store       *IntelligenceStore
	provider    intelligenceProvider
	baseURL     string
	apiBaseURLs map[int64]string
	now         func() time.Time
	tickMu      sync.Mutex
	configMu    sync.Mutex
}

func NewIntelligenceService(store *IntelligenceStore, mainOrigin, providerOrigin string) (*IntelligenceService, error) {
	return NewIntelligenceServiceWithAPIOrigins(store, mainOrigin, providerOrigin, "")
}

// NewIntelligenceServiceWithAPIOrigins builds the worker. providerOrigin is the
// detection service (INTELLIGENCE_PROVIDER_ORIGIN) and is required.
func NewIntelligenceServiceWithAPIOrigins(store *IntelligenceStore, mainOrigin, providerOrigin, rawOverrides string) (*IntelligenceService, error) {
	s, err := CheckIntelligenceService(store, mainOrigin, providerOrigin, rawOverrides)
	if err != nil {
		return nil, err
	}
	// The old owner has released portal.lock. An interrupted POST without a
	// durable remote ID is indeterminate, not a queue item that can be retried.
	hasInterrupted := false
	for _, r := range store.snapshot().Runs {
		hasInterrupted = hasInterrupted || r.Run.Status == "submitting"
	}
	if hasInterrupted {
		err = store.change(func(st *intelligenceState) error {
			for id, r := range st.Runs {
				if r.Run.Status == "submitting" {
					now := s.now()
					if r.Deadline.IsZero() {
						r.Deadline = r.Run.StartedAt.Add(12 * time.Minute)
					}
					r.Run.Status, r.Run.Phase, r.Run.Verdict = "unknown", "complete", "unknown"
					r.Run.ErrorCode, r.Run.FinishedAt = "submission_unknown", &now
					st.Runs[id] = r
				}
			}
			return nil
		})
	}
	return s, err
}

// CheckIntelligenceService validates the worker configuration exactly as
// NewIntelligenceServiceWithAPIOrigins does, but does not rewrite interrupted
// submissions: while the old owner still runs, a "submitting" run may be live.
// The result is never started; it holds no goroutine and makes no request.
func CheckIntelligenceService(store *IntelligenceStore, mainOrigin, providerOrigin, rawOverrides string) (*IntelligenceService, error) {
	origin, err := CanonicalOrigin(mainOrigin)
	if err != nil {
		return nil, err
	}
	provider, err := newIntelligenceProvider(providerOrigin)
	if err != nil {
		return nil, err
	}
	origins, err := intelligenceAPIOrigins(rawOverrides)
	if err != nil {
		return nil, err
	}
	return &IntelligenceService{Store: store, provider: provider, baseURL: origin + "/v1", apiBaseURLs: origins, now: func() time.Time { return time.Now().UTC() }}, nil
}

func intelligenceRequestHash(plan, benchmark, request string) string {
	sum := sha256.Sum256([]byte(plan + "\n" + benchmark + "\n" + request))
	return hex.EncodeToString(sum[:])
}

func (s *IntelligenceService) Enqueue(planID, benchmark, requestID string, manual bool) (IntelligenceRun, error) {
	var result IntelligenceRun
	if !intelligenceID.MatchString(planID) || (benchmark != "candy" && !intelligenceDrawing(benchmark)) || len(requestID) < 8 || len(requestID) > 160 {
		return result, ErrIntelligenceInvalid
	}
	now := s.now()
	hash := intelligenceRequestHash(planID, benchmark, requestID)
	err := s.Store.change(func(st *intelligenceState) error {
		if prior, ok := st.Requests[hash]; ok {
			if r, ok := st.Runs[prior.RunID]; ok {
				result = r.Run
				return nil
			}
			// A retained tombstone must not cause a paid replay after history pruning.
			return ErrConflict
		}
		p, ok := st.Plans[planID]
		if !ok {
			return ErrNotFound
		}
		if p.Plan.KeyStatus != "active" || p.Secret == "" || intelligenceKeyExpired(p.Plan.ExpiresAt, now) || (!manual && !p.Plan.Enabled) {
			return ErrCredential
		}
		seconds := p.Plan.CandySeconds
		if intelligenceDrawing(benchmark) {
			seconds = p.Plan.PelicanSeconds
		}
		if seconds == 0 {
			return ErrIntelligenceInvalid
		}
		for _, r := range st.Runs {
			if r.Run.PlanID == planID && intelligenceCategory(r.Run.Benchmark) == intelligenceCategory(benchmark) && intelligenceInFlight(r, now) {
				return ErrBusy
			}
		}
		trigger := "scheduled"
		if manual {
			trigger = "manual"
		}
		// Choose and persist the scheduled type inside the same transaction as
		// its queue item. Retrying the original operation returns before advancing.
		selected := benchmark
		if !manual && intelligenceDrawing(benchmark) {
			selected = intelligenceNextDrawing(p.Plan)
		}
		result = IntelligenceRun{ID: randomID(), PlanID: planID, Name: p.Plan.Name, Benchmark: selected, Model: p.Plan.Model, Effort: p.Plan.Effort, Status: "queued", Phase: "queued", Verdict: "unknown", StartedAt: now, Trigger: trigger}
		st.Runs[result.ID] = intelligenceRunRecord{Run: result, RequestHash: hash}
		st.Requests[hash] = intelligenceRequest{RunID: result.ID, At: now}
		if !manual {
			next := now.Add(time.Duration(seconds) * time.Second)
			if benchmark == "candy" {
				p.Plan.NextCandy = next
			} else {
				p.Plan.NextPelican = next
				if p.Plan.CreativeEveryFive {
					p.Plan.DrawingSequence++
				}
			}
			st.Plans[planID] = p
		}
		return nil
	})
	return result, err
}

func (s *IntelligenceService) schedule() {
	now := s.now()
	for id, p := range s.Store.snapshot().Plans {
		if p.Plan.KeyStatus == "active" && intelligenceKeyExpired(p.Plan.ExpiresAt, now) {
			_ = s.Store.change(func(st *intelligenceState) error {
				row, exists := st.Plans[id]
				if !exists || row.Plan.KeyStatus != "active" || !intelligenceKeyExpired(row.Plan.ExpiresAt, now) {
					return ErrConflict
				}
				row.Plan.Enabled, row.Plan.KeyStatus = false, "expired"
				row.Plan.Revision++
				st.Plans[id] = row
				return nil
			})
			continue
		}
		if !p.Plan.Enabled || p.Plan.KeyStatus != "active" {
			continue
		}
		for _, kind := range []struct {
			name    string
			next    time.Time
			seconds int
		}{{"candy", p.Plan.NextCandy, p.Plan.CandySeconds}, {"pelican", p.Plan.NextPelican, p.Plan.PelicanSeconds}} {
			if kind.seconds > 0 && !kind.next.After(now) {
				request := "scheduled-" + id + "-" + kind.name + "-" + kind.next.UTC().Format(time.RFC3339Nano)
				_, _ = s.Enqueue(id, kind.name, request, false)
			}
		}
	}
}

func (s *IntelligenceService) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.tick()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *IntelligenceService) tick() {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	s.schedule()
	state := s.Store.snapshot()
	rows := make([]intelligenceRunRecord, 0, len(state.Runs))
	active := 0
	for _, r := range state.Runs {
		if r.Run.Status == "running" || r.Run.Status == "submitting" || r.Run.Status == "unknown" && r.Deadline.After(s.now()) {
			active++
		}
		if r.Run.Status == "running" && !r.NextPoll.After(s.now()) || r.Run.Status == "queued" {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Run.StartedAt.Before(rows[j].Run.StartedAt) })
	var wg sync.WaitGroup
	for _, row := range rows {
		if row.Run.Status == "queued" {
			if active >= 2 || state.SubmitAfter.After(s.now()) {
				continue
			}
			active++
		}
		wg.Add(1)
		go func(r intelligenceRunRecord) {
			defer wg.Done()
			// Stop dispatching on shutdown; let already-started bounded HTTP calls
			// finish so accepted IDs/results can be persisted before lock release.
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if r.Run.Status == "queued" {
				s.submit(ctx, r.Run.ID)
			} else {
				s.poll(ctx, r)
			}
		}(row)
	}
	wg.Wait()
}

func (s *IntelligenceService) submit(ctx context.Context, id string) {
	var run intelligenceRunRecord
	var plan intelligencePlanRecord
	err := s.Store.change(func(st *intelligenceState) error {
		var ok bool
		run, ok = st.Runs[id]
		if !ok || run.Run.Status != "queued" {
			return ErrConflict
		}
		plan, ok = st.Plans[run.Run.PlanID]
		if !ok || plan.Secret == "" || plan.Plan.KeyStatus != "active" || intelligenceKeyExpired(plan.Plan.ExpiresAt, s.now()) || (!plan.Plan.Enabled && run.Run.Trigger != "manual") {
			now := s.now()
			run.Run.Status, run.Run.Phase, run.Run.ErrorCode, run.Run.FinishedAt = "failed", "complete", "test_paused", &now
			st.Runs[id] = run
			return nil
		}
		run.Run.Status, run.Run.Phase = "submitting", "generating"
		run.Deadline = s.now().Add(12 * time.Minute)
		st.Runs[id] = run
		return nil
	})
	if err != nil || run.Run.Status != "submitting" {
		return
	}
	key, err := s.Store.decrypt(plan)
	if err != nil {
		s.fail(id, "failed", "key_unavailable")
		return
	}
	out, err := s.provider.Submit(ctx, intelligenceSubmission{Benchmark: run.Run.Benchmark, BaseURL: s.apiBaseURL(plan.Plan.GroupID), APIKey: key, Model: run.Run.Model, Effort: run.Run.Effort}, "sources-"+id)
	if err != nil {
		code, definitive, delay := intelligenceErrorCode(err)
		if code == "test_service_busy" {
			_ = s.Store.change(func(st *intelligenceState) error {
				st.SubmitAfter = s.now().Add(delay)
				return nil
			})
		}
		status := "unknown"
		if definitive {
			status = "failed"
		}
		s.fail(id, status, code)
		return
	}
	if !intelligenceRemoteID.MatchString(out.ID) || out.Benchmark != "" && out.Benchmark != run.Run.Benchmark {
		s.fail(id, "unknown", "response_uncertain")
		return
	}
	err = s.Store.change(func(st *intelligenceState) error {
		r := st.Runs[id]
		if r.Run.Status != "submitting" {
			return ErrConflict
		}
		r.RemoteID, r.Run.Status, r.NextPoll = out.ID, "running", s.now().Add(5*time.Second)
		if !out.ExpiresAt.IsZero() && out.ExpiresAt.Before(r.Deadline) {
			r.Deadline = out.ExpiresAt
		}
		st.Runs[id] = r
		return nil
	})
	if err == nil {
		s.consume(id, out)
	}
}

func (s *IntelligenceService) poll(ctx context.Context, r intelligenceRunRecord) {
	if !r.Deadline.After(s.now()) {
		s.fail(r.Run.ID, "unknown", "result_timeout")
		return
	}
	out, err := s.provider.Poll(ctx, r.RemoteID)
	if err != nil {
		code, definitive, delay := intelligenceErrorCode(err)
		if definitive && code == "task_unavailable" {
			s.fail(r.Run.ID, "unknown", code)
			return
		}
		if delay < 5*time.Second {
			delay = 5 * time.Second
		}
		_ = s.Store.change(func(st *intelligenceState) error {
			row := st.Runs[r.Run.ID]
			row.NextPoll = s.now().Add(delay)
			st.Runs[r.Run.ID] = row
			return nil
		})
		return
	}
	if out.ID != r.RemoteID || out.Benchmark != "" && out.Benchmark != r.Run.Benchmark {
		s.fail(r.Run.ID, "unknown", "response_uncertain")
		return
	}
	s.consume(r.Run.ID, out)
}

func nonnegativeMetric(v *int64) *int64 {
	if v == nil || *v < 0 || *v > 1e12 {
		return nil
	}
	n := *v
	return &n
}

func (s *IntelligenceService) consume(id string, out intelligenceRemoteResult) {
	if out.Status == "queued" || out.Status == "running" {
		_ = s.Store.change(func(st *intelligenceState) error {
			r := st.Runs[id]
			r.Run.Phase = "generating"
			if out.Phase == "classifying" {
				r.Run.Phase = "classifying"
			}
			r.NextPoll = s.now().Add(5 * time.Second)
			st.Runs[id] = r
			return nil
		})
		return
	}
	if out.Status == "failed" || out.Status == "cancelled" {
		raw := append(append([]byte{}, out.Error...), out.Candy.Error...)
		s.fail(id, "failed", intelligenceFailure(raw))
		return
	}
	if out.Status != "succeeded" {
		s.fail(id, "unknown", "response_uncertain")
		return
	}
	r, ok := s.Store.snapshot().Runs[id]
	if !ok || r.Run.Status != "running" {
		return
	}
	result := IntelligenceResult{}
	verdict := "unknown"
	var duration, input, output *int64
	if r.Run.Benchmark == "candy" {
		result.Answer = out.Candy.Answer
		if strings.TrimSpace(result.Answer) != "" && (out.Candy.Status == "passed" || out.Candy.Status == "incorrect") {
			verdict = out.Candy.Status
		}
		duration, input, output = out.Candy.DurationMS, out.Candy.InputTokens, out.Candy.OutputTokens
	} else {
		result.HTML = out.Result.HTML
		if strings.TrimSpace(result.HTML) != "" {
			switch out.Assessment.Quality {
			case "normal":
				verdict = "normal"
			case "degraded", "suspicious":
				verdict = "degraded"
			}
		}
		duration, input, output = out.Result.DurationMS, out.Result.InputTokens, out.Result.OutputTokens
	}
	if err := s.Store.saveResult(id, result); err != nil {
		if errors.Is(err, ErrUnsafeIntelligenceResult) {
			s.fail(id, "failed", "result_unavailable")
		}
		// The remote ID remains durable; retry only the GET and result persistence.
		return
	}
	err := s.Store.change(func(st *intelligenceState) error {
		row := st.Runs[id]
		if row.Run.Status != "running" {
			return ErrConflict
		}
		now := s.now()
		row.Run.Status, row.Run.Phase, row.Run.Verdict = "succeeded", "complete", verdict
		row.Run.FinishedAt = &now
		row.Run.DurationMS, row.Run.InputTokens, row.Run.OutputTokens = nonnegativeMetric(duration), nonnegativeMetric(input), nonnegativeMetric(output)
		row.Run.HasResult, row.Run.HasHTML = true, result.HTML != ""
		if verdict == "unknown" {
			row.Run.ErrorCode = "assessment_unknown"
		}
		st.Runs[id] = row
		return nil
	})
	if err == nil {
		_ = s.Store.prune(s.now())
	}
}

func (s *IntelligenceService) fail(id, status, code string) {
	err := s.Store.change(func(st *intelligenceState) error {
		r, ok := st.Runs[id]
		if !ok || !intelligenceActive(r.Run.Status) {
			return ErrConflict
		}
		now := s.now()
		r.Run.Status, r.Run.Phase, r.Run.Verdict = status, "complete", "unknown"
		r.Run.ErrorCode, r.Run.FinishedAt = code, &now
		st.Runs[id] = r
		if code == "quota_exhausted" || code == "key_unavailable" {
			p := st.Plans[r.Run.PlanID]
			p.Plan.Enabled, p.Plan.KeyStatus = false, code
			p.Plan.Revision++
			st.Plans[r.Run.PlanID] = p
		}
		return nil
	})
	if err == nil || errors.Is(err, ErrConflict) {
		_ = s.Store.prune(s.now())
	}
}
