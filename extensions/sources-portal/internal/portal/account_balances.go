package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrSourceLinked = errors.New("source_has_account_bindings")
var ErrUnverifiedSource = errors.New("source_requires_successful_identity_check")

type AccountBinding struct {
	MainOrigin string `json:"main_origin"`
	AccountID  int64  `json:"account_id"`
	SourceID   string `json:"source_id"`
	Marker     string `json:"identity_marker"`
	Revision   int64  `json:"revision"`
}
type bindingState struct {
	Version  int                       `json:"version"`
	Bindings map[string]AccountBinding `json:"bindings"`
}
type BalanceAccountIdentity struct {
	AccountID int64  `json:"account_id"`
	Supported bool   `json:"supported"`
	Marker    string `json:"marker,omitempty"`
}
type AccountBalanceSummary struct {
	AccountID       int64      `json:"account_id"`
	Supported       bool       `json:"supported"`
	Status          string     `json:"status"`
	SourceID        string     `json:"source_id,omitempty"`
	SourceName      string     `json:"source_name,omitempty"`
	BindingRevision int64      `json:"binding_revision"`
	SharedCount     int        `json:"shared_count"`
	Balance         *Balance   `json:"balance"`
	LastCheckedAt   *time.Time `json:"last_checked_at"`
	Stale           bool       `json:"stale"`
	Error           string     `json:"error,omitempty"`
}

func bindingKey(origin string, id int64) string { return origin + "\n" + strconv.FormatInt(id, 10) }
func (s *Store) loadBindings() error {
	s.bindings = bindingState{Version: 1, Bindings: map[string]AccountBinding{}}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), "account-bindings.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &s.bindings) != nil || s.bindings.Version != 1 || s.bindings.Bindings == nil {
		return errors.New("invalid_account_bindings")
	}
	for key, b := range s.bindings.Bindings {
		origin, e := CanonicalOrigin(b.MainOrigin)
		if e != nil || origin != b.MainOrigin || b.AccountID <= 0 || b.Revision <= 0 || key != bindingKey(origin, b.AccountID) || (b.SourceID != "" && (!validIdentityMarker(b.Marker))) {
			return errors.New("invalid_account_binding")
		}
	}
	return nil
}
func validIdentityMarker(v string) bool {
	if len(v) != 67 || !strings.HasPrefix(v, "v1:") {
		return false
	}
	for _, c := range v[3:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *Store) setBinding(origin string, id int64, identity BalanceAccountIdentity, sourceID string, sourceRevision, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	key := bindingKey(origin, id)
	old := s.bindings.Bindings[key]
	if old.Revision != revision {
		return ErrConflict
	}
	if sourceID != "" {
		rec, ok := s.state.Records[sourceID]
		if !ok {
			return ErrNotFound
		}
		if rec.Source.Revision != sourceRevision {
			return ErrConflict
		}
		if identity.AccountID != id || !identity.Supported || !validIdentityMarker(identity.Marker) {
			return ErrIdentity
		}
		if (rec.Source.Status != "ok" && rec.Source.Status != "low") || rec.Source.LastBalance == nil || rec.Source.LastBalance.Unit == "" || rec.Source.AccountID <= 0 || rec.Source.AccountID != rec.Source.LastBalance.AccountID {
			return ErrUnverifiedSource
		}
	}
	next := bindingState{Version: 1, Bindings: make(map[string]AccountBinding, len(s.bindings.Bindings)+1)}
	for k, v := range s.bindings.Bindings {
		next.Bindings[k] = v
	}
	// Keep a revision tombstone after unlink, so stale editors cannot recreate a link.
	next.Bindings[key] = AccountBinding{MainOrigin: origin, AccountID: id, SourceID: sourceID, Marker: identity.Marker, Revision: revision + 1}
	err := writeJSONAtomic(filepath.Join(filepath.Dir(s.path), "account-bindings.json"), next)
	if err == nil || writeCommitted(err) {
		s.bindings = next
	}
	if writeCommitted(err) {
		s.failed = ErrPersistenceUncertain
	}
	return err
}
func (s *Store) summaries(origin string, ids []int64, identities map[int64]BalanceAccountIdentity, now time.Time) []AccountBalanceSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	shared := map[string]int{}
	for _, b := range s.bindings.Bindings {
		if b.MainOrigin == origin && b.SourceID != "" {
			shared[b.SourceID]++
		}
	}
	out := make([]AccountBalanceSummary, 0, len(ids))
	for _, id := range ids {
		identity, found := identities[id]
		b := s.bindings.Bindings[bindingKey(origin, id)]
		item := AccountBalanceSummary{AccountID: id, Supported: identity.Supported, Status: "unbound", BindingRevision: b.Revision}
		if !found {
			item.Status = "not_found"
			out = append(out, item)
			continue
		}
		if !identity.Supported {
			item.Status = "unsupported"
			out = append(out, item)
			continue
		}
		if b.SourceID == "" {
			out = append(out, item)
			continue
		}
		item.SourceID = b.SourceID
		item.SharedCount = shared[b.SourceID]
		rec, ok := s.state.Records[b.SourceID]
		if !ok || b.Marker != identity.Marker {
			item.Status = "rebind_required"
			out = append(out, item)
			continue
		}
		src := rec.Source
		item.SourceName = src.Name
		item.Status = src.Status
		item.Error = src.Error
		if item.Status == "ok" {
			item.Status = "ready"
		}
		if !src.LastCheckedAt.IsZero() {
			t := src.LastCheckedAt
			item.LastCheckedAt = &t
		}
		if src.LastBalance != nil {
			balance := *src.LastBalance
			item.Balance = &balance
			item.Stale = src.Status == "error" || src.Status == "pending" || now.Sub(balance.CheckedAt) > 6*time.Minute
		}
		if item.Balance != nil && item.Balance.Unit == "" || src.Status == "unit_mismatch" {
			item.Status = "unit_unconfirmed"
		}
		out = append(out, item)
	}
	return out
}

func parseBalanceIDs(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > 100 {
		return nil, errors.New("invalid_account_ids")
	}
	ids := make([]int64, 0, len(parts))
	seen := map[int64]bool{}
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part || seen[id] {
			return nil, errors.New("invalid_account_ids")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}
func idsQuery(ids []int64) string {
	p := make([]string, len(ids))
	for i, id := range ids {
		p[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(p, ",")
}
func (s *Server) balanceIdentities(r *http.Request, ids []int64) (map[int64]BalanceAccountIdentity, error) {
	var reply struct {
		Accounts []BalanceAccountIdentity `json:"accounts"`
	}
	if err := s.mainAPI(r, "GET", "/api/v1/admin/accounts/balance-identities?ids="+idsQuery(ids), nil, "", &reply); err != nil {
		return nil, err
	}
	result := map[int64]BalanceAccountIdentity{}
	requested := map[int64]bool{}
	for _, id := range ids {
		requested[id] = true
	}
	for _, v := range reply.Accounts {
		if !requested[v.AccountID] || result[v.AccountID].AccountID != 0 || v.Supported && !validIdentityMarker(v.Marker) {
			return nil, ErrResponse
		}
		result[v.AccountID] = v
	}
	return result, nil
}
func (s *Server) balanceAccountRoutes(mux *http.ServeMux) {
	read := func(w http.ResponseWriter, r *http.Request, ids []int64) {
		if s.Service.Bridge != nil {
			var reply json.RawMessage
			if err := s.mainAPI(r, "GET", "/api/v1/admin/accounts/wallet-summaries?ids="+idsQuery(ids), nil, "", &reply); err != nil {
				apiError(w, err)
				return
			}
			jsonResponse(w, 200, reply)
			return
		}

		identities, err := s.balanceIdentities(r, ids)
		if err != nil {
			jsonResponse(w, 503, map[string]string{"error": "account_identity_unavailable"})
			return
		}
		jsonResponse(w, 200, map[string]any{"accounts": s.Service.Store.summaries(s.SubOrigin, ids, identities, time.Now())})
	}
	mux.HandleFunc("GET /api/balance/accounts", func(w http.ResponseWriter, r *http.Request) {
		ids, err := parseBalanceIDs(r.URL.Query().Get("ids"))
		if err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_account_ids"})
			return
		}
		read(w, r, ids)
	})
	bind := func(w http.ResponseWriter, r *http.Request) {
		ids, err := parseBalanceIDs(r.PathValue("id"))
		if err != nil || len(ids) != 1 {
			jsonResponse(w, 400, map[string]string{"error": "invalid_account_id"})
			return
		}
		var in struct {
			SourceID       string `json:"source_id"`
			SourceRevision int64  `json:"source_revision"`
			Revision       int64  `json:"binding_revision"`
		}
		if decode(w, r, &in) != nil || in.Revision < 0 || r.Method == "PUT" && in.SourceID == "" || r.Method == "DELETE" && (in.SourceID != "" || in.SourceRevision != 0) {
			jsonResponse(w, 400, map[string]string{"error": "invalid_binding"})
			return
		}

		if s.Service.Bridge != nil {
			if err := s.Service.Bridge.Push(r.Context(), s.Service.Store); err != nil {
				apiError(w, err)
				return
			}
			var reply json.RawMessage
			if err := s.mainAPI(r, "PUT", "/api/v1/admin/accounts/wallet-source-binding?id="+idsQuery(ids), in, "", &reply); err != nil {
				apiError(w, err)
				return
			}
			jsonResponse(w, 200, reply)
			return
		}
		identities, err := s.balanceIdentities(r, ids)
		if err != nil {
			jsonResponse(w, 503, map[string]string{"error": "account_identity_unavailable"})
			return
		}
		if err = s.Service.Store.setBinding(s.SubOrigin, ids[0], identities[ids[0]], in.SourceID, in.SourceRevision, in.Revision); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, s.Service.Store.summaries(s.SubOrigin, ids, identities, time.Now())[0])
	}
	mux.HandleFunc("PUT /api/balance/accounts/{id}/binding", bind)
	mux.HandleFunc("DELETE /api/balance/accounts/{id}/binding", bind)
	mux.HandleFunc("POST /api/balance/accounts/refresh", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IDs []int64 `json:"account_ids"`
		}
		if decode(w, r, &in) != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_account_ids"})
			return
		}
		ids, err := parseBalanceIDs(idsQuery(in.IDs))
		if err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_account_ids"})
			return
		}
		identities, err := s.balanceIdentities(r, ids)
		if err != nil {
			jsonResponse(w, 503, map[string]string{"error": "account_identity_unavailable"})
			return
		}
		rows := s.Service.Store.summaries(s.SubOrigin, ids, identities, time.Now())
		sources := map[string]bool{}
		for _, item := range rows {
			if item.SourceID != "" && item.Status != "rebind_required" && item.Supported {
				sources[item.SourceID] = true
			}
		}
		refreshed := []string{}
		failures := map[string]string{}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(balanceOperationTimeout))
		ctx, cancel := context.WithTimeout(r.Context(), balanceOperationTimeout)
		defer cancel()
		var mu sync.Mutex
		var work sync.WaitGroup
		jobs := make(chan string)
		for i := 0; i < 2; i++ {
			work.Add(1)
			go func() {
				defer work.Done()
				for sourceID := range jobs {
					var result Source
					var failure error
					if ctx.Err() != nil {
						failure = balanceQueryError(ctx, ctx.Err(), ErrTransport)
					} else {
						result, failure = s.Service.Refresh(ctx, sourceID)
					}
					mu.Lock()
					if failure != nil {
						failures[sourceID] = balanceRefreshError(failure)
					} else if result.Status == "ok" || result.Status == "low" {
						refreshed = append(refreshed, sourceID)
					}
					mu.Unlock()
				}
			}()
		}
		for sourceID := range sources {
			jobs <- sourceID
		}
		close(jobs)
		work.Wait()

		// Do not expose an old wallet after a concurrent model-key edit.
		identities, err = s.balanceIdentities(r, ids)
		if err != nil {
			jsonResponse(w, 503, map[string]string{"error": "account_identity_unavailable"})
			return
		}
		rows = s.Service.Store.summaries(s.SubOrigin, ids, identities, time.Now())
		for i := range rows {
			if failure := failures[rows[i].SourceID]; failure != "" && rows[i].Status != "rebind_required" {
				rows[i].Error = failure
				rows[i].Stale = rows[i].Balance != nil
			}
		}
		sort.Strings(refreshed)
		jsonResponse(w, 200, map[string]any{"accounts": rows, "refreshed_source_ids": refreshed})
	})
}

// Upstream errors are already classified by the provider. Persistence failures
// must not expose filesystem paths or OS diagnostics in a table cell.
func balanceRefreshError(err error) string {
	for _, known := range []error{ErrNotFound, ErrConflict, ErrBusy, ErrCredential, ErrTransport, ErrQueryTimeout} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "storage_unavailable"
}
