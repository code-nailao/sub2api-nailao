package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("source_not_found")
var ErrConflict = errors.New("source_changed_refresh_page")
var ErrBusy = errors.New("refresh_in_progress_or_rate_limited")
var ErrSourceDeletionReservation = errors.New("source_deletion_reservation_required")

type SourceInput struct {
	Name          string `json:"name"`
	Origin        string `json:"origin"`
	AccountID     int64  `json:"account_id"`
	PersonalToken string `json:"personal_token"`
	Threshold     string `json:"threshold"`
	ThresholdUnit string `json:"threshold_unit"`
	Revision      int64  `json:"revision"`
}

const balanceConcurrency = 20
const balanceSiteConcurrency = 4

type Service struct {
	Bridge       *WalletBridge
	Intelligence *IntelligenceService
	Store        *Store
	Provider     Provider
	Interval     time.Duration
	Webhook      string
	NotifyClient *http.Client
	mu           sync.Mutex
	notifyMu     sync.Mutex
	inflight     map[string]bool
	last         map[string]time.Time
	gate         chan struct{}
	sites        map[string]int
	asyncCtx     context.Context
	asyncCancel  context.CancelFunc
	asyncJobs    map[string]*balanceAsyncJob
	asyncActive  int
	asyncClosing bool
	asyncWorkers sync.WaitGroup
}

func NewService(s *Store, p Provider) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{asyncCtx: ctx, asyncCancel: cancel, asyncJobs: map[string]*balanceAsyncJob{}, Store: s, Provider: p, Interval: 5 * time.Minute, inflight: map[string]bool{}, last: map[string]time.Time{}, gate: make(chan struct{}, balanceConcurrency), sites: map[string]int{}}
}
func (s *Service) List() []Source {
	out := []Source{}
	for _, r := range s.Store.snapshot() {
		src := r.Source
		src.AlertPending = r.Pending != nil
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (s *Service) Save(id string, in SourceInput, adminID int64) (Source, error) {
	var result Source
	in.Name = strings.TrimSpace(in.Name)
	in.PersonalToken = strings.TrimSpace(in.PersonalToken)
	in.Threshold = strings.TrimSpace(in.Threshold)
	in.ThresholdUnit = strings.TrimSpace(in.ThresholdUnit)
	if in.Name == "" || len(in.Name) > 160 || in.AccountID < 0 {
		return result, errors.New("invalid_name_or_user_id")
	}
	origin, err := CanonicalOrigin(in.Origin)
	if err != nil {
		return result, err
	}
	if in.Threshold != "" {
		x, ok := amountRat(in.Threshold)
		if !ok || x.Sign() < 0 || len(in.Threshold) > 32 || len(in.ThresholdUnit) == 0 || len(in.ThresholdUnit) > 16 {
			return result, errors.New("invalid_alert_threshold_or_unit")
		}
	}
	if strings.HasPrefix(in.PersonalToken, "sk-") || strings.ContainsAny(in.PersonalToken, "\r\n") || len(in.PersonalToken) > 8192 {
		return result, ErrCredential
	}
	isNew := id == ""
	if isNew {
		id = randomID()
	}
	err = s.Store.change(func(records map[string]record) error {
		rec, found := records[id]
		if isNew {
			if in.PersonalToken == "" {
				return ErrCredential
			}
			rec.Source = Source{ID: id, Origin: origin, Status: "pending", AccountID: in.AccountID}
		} else {
			if !found {
				return ErrNotFound
			}
			if in.Revision != rec.Source.Revision {
				return ErrConflict
			}
			if origin != rec.Source.Origin || (rec.Source.AccountID != 0 && in.AccountID != 0 && in.AccountID != rec.Source.AccountID) {
				return errors.New("site_or_account_is_immutable_create_another_source")
			}
		}
		if rec.Source.AccountID == 0 {
			rec.Source.AccountID = in.AccountID
		}
		if rec.Source.Threshold != in.Threshold || rec.Source.ThresholdUnit != in.ThresholdUnit || in.PersonalToken != "" {
			rec.Pending = nil
			rec.Alarm = ""
			rec.Source.Status = "pending"
			rec.Source.Error = ""
		}
		rec.Source.Name = in.Name
		rec.Source.Threshold = in.Threshold
		rec.Source.ThresholdUnit = in.ThresholdUnit
		rec.Source.UpdatedBy = adminID
		rec.Source.Revision++
		if in.PersonalToken != "" {
			rec.Secret = s.Store.encrypt(rec.Source, in.PersonalToken)
		}
		rec.Source.NextCheckAt = time.Time{}
		records[id] = rec
		result = rec.Source
		return nil
	})
	return result, err
}
func (s *Service) Delete(id string, rev int64) error {
	if s.Bridge != nil {
		return ErrSourceDeletionReservation
	}
	return s.deleteSource(id, rev, false)
}

type sourceDeletionReservation struct {
	SourceID       string `json:"source_id"`
	SourceRevision int64  `json:"source_revision"`
	Reserved       bool   `json:"reserved"`
}

func (s *Service) deleteReservedSource(id string, rev int64, reservation sourceDeletionReservation) error {
	if s.Bridge == nil || rev <= 0 || !reservation.Reserved || reservation.SourceID != id || reservation.SourceRevision != rev {
		return ErrSourceDeletionReservation
	}
	return s.deleteSource(id, rev, true)
}

func (s *Service) deleteSource(id string, rev int64, canonicalReservation bool) error {
	return s.Store.change(func(rows map[string]record) error {
		r, ok := rows[id]
		if !ok {
			return ErrNotFound
		}
		if r.Source.Revision != rev {
			return ErrConflict
		}
		if !canonicalReservation {
			for _, binding := range s.Store.bindings.Bindings {
				if binding.SourceID == id {
					return ErrSourceLinked
				}
			}
		}
		delete(rows, id)
		return nil
	})
}
func (s *Service) Refresh(ctx context.Context, id string) (Source, error) {
	source, ok := s.Store.snapshot()[id]
	if !ok {
		return Source{}, ErrNotFound
	}
	origin, _ := url.Parse(source.Source.Origin)
	site := strings.ToLower(origin.Hostname())
	s.mu.Lock()
	if s.inflight[id] || time.Since(s.last[id]) < 15*time.Second || s.sites[site] >= balanceSiteConcurrency {
		s.mu.Unlock()
		return Source{}, ErrBusy
	}
	select {
	case s.gate <- struct{}{}:
	default:
		s.mu.Unlock()
		return Source{}, ErrBusy
	}
	s.inflight[id] = true
	s.last[id] = time.Now()
	s.sites[site]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.inflight, id)
		s.sites[site]--
		if s.sites[site] == 0 {
			delete(s.sites, site)
		}
		<-s.gate
	}()
	rec, ok := s.Store.snapshot()[id]
	if !ok {
		return Source{}, ErrNotFound
	}
	token, err := s.Store.decrypt(rec)
	if err != nil {
		return Source{}, ErrCredential
	}
	ctx, cancel := context.WithTimeout(ctx, balanceOperationTimeout)
	defer cancel()
	// Balance probes are read-only and can stop at process shutdown, including
	// synchronous HTTP refreshes whose request context is still alive.
	stop := context.AfterFunc(s.asyncCtx, cancel)
	defer stop()
	if s.asyncCtx.Err() != nil {
		cancel()
	}
	balance, probeErr := s.Provider.Probe(ctx, rec.Source.Origin, token, rec.Source.AccountID)
	var result Source
	err = s.Store.change(func(rows map[string]record) error {
		current, ok := rows[id]
		if !ok {
			return ErrNotFound
		}
		if current.Source.Revision != rec.Source.Revision {
			return ErrConflict
		}
		now := time.Now().UTC()
		current.Source.LastCheckedAt = now
		current.Source.NextCheckAt = now.Add(s.Interval)
		if probeErr != nil {
			current.Source.Status = "error"
			current.Source.Error = probeErr.Error()
			current.Source.NextCheckAt = now.Add(5 * time.Minute)
		} else {
			for key, other := range rows {
				if key != id && other.Source.Origin == current.Source.Origin && other.Source.AccountID == balance.AccountID {
					current.Source.Status = "error"
					current.Source.Error = "account_already_monitored"
					current.Source.NextCheckAt = now.Add(s.Interval)
					rows[id] = current
					result = current.Source
					return nil
				}
			}
			current.Source.LastBalance = &balance
			current.Source.AccountID = balance.AccountID
			current.Source.Status = "ok"
			current.Source.Error = ""
			if current.Source.Threshold != "" && s.Bridge == nil {
				if current.Source.ThresholdUnit != balance.Unit {
					current.Source.Status = "unit_mismatch"
					current.Source.Error = "alert_unit_differs_from_upstream"
				} else {
					amount, _ := new(big.Rat).SetString(balance.Remaining)
					threshold, _ := new(big.Rat).SetString(current.Source.Threshold)
					if amount.Cmp(threshold) <= 0 {
						current.Source.Status = "low"
					}
				}
			}
			if current.Source.Status == "ok" || current.Source.Status == "low" {
				next := current.Source.Status
				if next != current.Alarm && (next == "low" || current.Alarm == "low") && s.Webhook != "" {
					current.Pending = &Notice{ID: randomID(), SourceID: id, Name: current.Source.Name, State: next, Balance: balance}
				}
				current.Alarm = next
			}
		}
		rows[id] = current
		result = current.Source
		return nil
	})
	if err != nil {
		return Source{}, err
	}
	if s.Bridge != nil {
		if err := s.Bridge.Push(ctx, s.Store); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (s *Service) Deliver(ctx context.Context, id string) error {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	rec, ok := s.Store.snapshot()[id]
	if !ok || rec.Pending == nil || s.Webhook == "" {
		return nil
	}
	b, _ := json.Marshal(rec.Pending)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Webhook, bytes.NewReader(b))
	if err != nil {
		return errors.New("notification_unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", rec.Pending.ID)
	res, err := s.NotifyClient.Do(req)
	if err != nil {
		return errors.New("notification_unavailable")
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("notification_unavailable")
	}
	return s.Store.change(func(rows map[string]record) error {
		current, ok := rows[id]
		if ok && current.Pending != nil && current.Pending.ID == rec.Pending.ID {
			current.Pending = nil
			rows[id] = current
		}
		return nil
	})
}
func (s *Service) balanceDue(src Source, now time.Time) bool {
	// New/edited credentials must be manually verified before periodic reads.
	if src.LastBalance == nil || src.Status == "pending" {
		return false
	}
	due := src.NextCheckAt
	// Old snapshots may still carry the previous 30-minute schedule.
	if next := src.LastCheckedAt.Add(s.Interval); due.IsZero() || next.Before(due) {
		due = next
	}
	return !due.After(now)
}

func (s *Service) Run(ctx context.Context) {
	defer s.StopAsync()
	// Do not wait for the intelligence worker before cancelling balance reads.
	stop := context.AfterFunc(ctx, s.asyncCancel)
	defer stop()
	var workers sync.WaitGroup
	if s.Bridge != nil {
		workers.Add(1)
		go func() { defer workers.Done(); s.Bridge.Run(ctx, s.Store) }()
	}
	if s.Intelligence != nil {
		workers.Add(1)
		go func() { defer workers.Done(); s.Intelligence.Run(ctx) }()
	}
	defer workers.Wait()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		rows := s.List()
		jobs := make(chan string)
		var balanceWorkers sync.WaitGroup
		for i := 0; i < balanceConcurrency; i++ {
			balanceWorkers.Add(1)
			go func() {
				defer balanceWorkers.Done()
				for id := range jobs {
					_, _ = s.Refresh(ctx, id)
				}
			}()
		}
	send:
		for _, src := range rows {
			if s.balanceDue(src, time.Now()) {
				select {
				case jobs <- src.ID:
				case <-ctx.Done():
					break send
				}
			}
		}
		close(jobs)
		balanceWorkers.Wait()
		for _, src := range rows {
			if ctx.Err() != nil {
				return
			}
			_ = s.Deliver(ctx, src.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
