package portal

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type intelligencePlanInput struct {
	Visibility        *string `json:"visibility"`
	CreativeEveryFive *bool   `json:"creative_every_five"`
	Revision          int64   `json:"revision"`
	GroupID           int64   `json:"group_id"`
	Name              string  `json:"name"`
	Model             string  `json:"model"`
	Effort            string  `json:"reasoning_effort"`
	Enabled           bool    `json:"enabled"`
	CandySeconds      int     `json:"candy_interval_seconds"`
	PelicanSeconds    int     `json:"pelican_interval_seconds"`
	DailyLimit        float64 `json:"daily_limit"`
	Quota             float64 `json:"quota"`
	ExpiresDays       int     `json:"expires_in_days"`
}

type intelligenceNativeKey struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	GroupID    int64      `json:"group_id"`
	Name       string     `json:"name"`
	Key        string     `json:"key"`
	Status     string     `json:"status"`
	Quota      float64    `json:"quota"`
	QuotaUsed  float64    `json:"quota_used"`
	DailyLimit float64    `json:"rate_limit_1d"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func validateIntelligenceInput(in intelligencePlanInput) error {
	if in.Visibility != nil && *in.Visibility != "admin" && *in.Visibility != "authenticated" {
		return ErrIntelligenceInvalid
	}
	interval := func(v int) bool { return v == 0 || v >= 60 && v <= 86400 }
	if in.GroupID <= 0 || len(in.Name) > 180 || in.Model != intelligenceModel || (in.Effort != "low" && in.Effort != "medium" && in.Effort != "high") ||
		!interval(in.CandySeconds) || !interval(in.PelicanSeconds) || in.CandySeconds+in.PelicanSeconds == 0 ||
		math.IsNaN(in.DailyLimit) || math.IsInf(in.DailyLimit, 0) || math.IsNaN(in.Quota) || math.IsInf(in.Quota, 0) ||
		in.DailyLimit <= 0 || in.Quota < 0 || (in.Quota > 0 && in.Quota < in.DailyLimit) || in.Quota > 100000 || in.ExpiresDays < 0 || in.ExpiresDays > 90 {
		return ErrIntelligenceInvalid
	}
	return nil
}

func (s *Server) intelligenceGroups(r *http.Request) ([]StudioGroup, error) {
	var groups []StudioGroup
	if err := s.mainAPI(r, "GET", "/api/v1/groups/available", nil, "", &groups); err != nil {
		return nil, err
	}
	out := []StudioGroup{}
	for _, group := range groups {
		if group.Status == "active" && group.Platform == "openai" {
			out = append(out, group)
		}
	}
	return out, nil
}

func (s *Server) saveIntelligencePlan(r *http.Request, id string, in intelligencePlanInput) (IntelligencePlan, error) {
	s.Intelligence.configMu.Lock()
	defer s.Intelligence.configMu.Unlock()
	var zero IntelligencePlan
	if in.Model == "" {
		in.Model = intelligenceModel
	}
	if in.Effort == "" {
		in.Effort = "medium"
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := validateIntelligenceInput(in); err != nil {
		return zero, err
	}
	user := r.Context().Value(identityKey{}).(Identity)
	store := s.Intelligence.Store
	if id != "" {
		record, ok := store.snapshot().Plans[id]
		if !ok || !intelligenceID.MatchString(id) {
			return zero, ErrNotFound
		}
		if record.Plan.Revision != in.Revision {
			return zero, ErrConflict
		}
		if record.Plan.OwnerID != user.ID || record.Plan.GroupID != in.GroupID {
			return zero, ErrCredential
		}
		if intelligenceVisibilityOnly(record.Plan, in) || intelligencePauseOnly(record.Plan, in) {
			// Hiding results or pausing execution must remain possible after
			// the native group/key becomes unavailable. Resuming still validates it.
			// Keep the latest scheduling state and encrypted credential intact.
			var result IntelligencePlan
			err := store.change(func(st *intelligenceState) error {
				current, exists := st.Plans[id]
				if !exists || current.Plan.Revision != in.Revision {
					return ErrConflict
				}
				if in.Visibility != nil {
					current.Plan.Visibility = *in.Visibility
				}
				current.Plan.Enabled = in.Enabled
				current.Plan.Revision++
				current.Plan.UpdatedAt = s.Intelligence.now()
				st.Plans[id] = current
				result = current.Plan
				return nil
			})
			return result, err
		}
	}
	groups, err := s.intelligenceGroups(r)
	if err != nil {
		return zero, err
	}
	found := false
	for _, group := range groups {
		if group.ID == in.GroupID {
			found = true
			if in.Name == "" {
				in.Name = group.Name
			}
		}
	}
	if !found || len(in.Name) > 180 {
		return zero, ErrCredential
	}
	if id == "" {
		id = randomID()
		if in.ExpiresDays == 0 {
			in.ExpiresDays = 30
		}
		err = store.change(func(st *intelligenceState) error {
			if len(st.Plans) >= 32 {
				return ErrIntelligenceInvalid
			}
			for _, existing := range st.Plans {
				if existing.Plan.GroupID == in.GroupID {
					return ErrConflict
				}
			}
			plan := IntelligencePlan{Visibility: "admin", ID: id, Revision: 1, GroupID: in.GroupID, Name: in.Name, Model: in.Model, Effort: in.Effort, CandySeconds: in.CandySeconds, PelicanSeconds: in.PelicanSeconds, OwnerID: user.ID, DailyLimit: in.DailyLimit, Quota: in.Quota, KeyStatus: "creating", UpdatedAt: s.Intelligence.now(), CreatedAt: s.Intelligence.now()}
			st.Plans[id] = intelligencePlanRecord{Plan: plan, Operation: randomID()}
			return nil
		})
		if err != nil {
			return zero, err
		}
		in.Revision = 1
	}
	record, ok := store.snapshot().Plans[id]
	if !ok || !intelligenceID.MatchString(id) {
		return zero, ErrNotFound
	}
	if record.Plan.Revision != in.Revision {
		return zero, ErrConflict
	}
	if record.Plan.OwnerID != user.ID || record.Plan.GroupID != in.GroupID {
		return zero, ErrCredential
	}
	key, err := s.intelligenceKey(r, record, in)
	if err != nil {
		return zero, err
	}
	if key.ID <= 0 || key.UserID != user.ID || key.GroupID != in.GroupID || key.Key == "" || len(key.Key) > 8192 || strings.ContainsAny(key.Key, "\r\n") {
		return zero, ErrCredential
	}
	// Native keys use quota=0 and expires_at=null for unlimited totals and
	// lifetime. The positive daily limit remains the monitoring spend boundary.
	usable := key.Status == "active" && !intelligenceKeyExpired(key.ExpiresAt, s.Intelligence.now()) && key.Quota >= 0 && (key.Quota == 0 || key.QuotaUsed < key.Quota) && key.DailyLimit > 0
	if in.Enabled && !usable {
		return zero, ErrCredential
	}
	ciphertext, err := store.encrypt(record.Plan, key.Key)
	if err != nil {
		return zero, err
	}
	var result IntelligencePlan
	err = store.change(func(st *intelligenceState) error {
		current, exists := st.Plans[id]
		if !exists || current.Plan.Revision != in.Revision {
			return ErrConflict
		}
		wasEnabled := current.Plan.Enabled
		current.Plan.Name, current.Plan.Model, current.Plan.Effort = in.Name, in.Model, in.Effort
		current.Plan.Enabled = in.Enabled
		if in.Visibility != nil {
			current.Plan.Visibility = *in.Visibility
		}
		if in.CreativeEveryFive != nil {
			current.Plan.CreativeEveryFive = *in.CreativeEveryFive
		}
		current.Plan.CandySeconds, current.Plan.PelicanSeconds = in.CandySeconds, in.PelicanSeconds
		current.Plan.KeyID, current.Plan.ExpiresAt = key.ID, key.ExpiresAt
		current.Plan.DailyLimit, current.Plan.Quota = key.DailyLimit, key.Quota
		current.Plan.KeyStatus = key.Status
		if key.Quota > 0 && key.QuotaUsed >= key.Quota {
			current.Plan.KeyStatus = "quota_exhausted"
		} else if intelligenceKeyExpired(key.ExpiresAt, s.Intelligence.now()) {
			current.Plan.KeyStatus = "expired"
		}
		current.Plan.UpdatedAt = s.Intelligence.now()
		if !wasEnabled && in.Enabled {
			current.Plan.NextCandy, current.Plan.NextPelican = current.Plan.UpdatedAt, current.Plan.UpdatedAt
		} else {
			if record.Plan.CandySeconds != in.CandySeconds {
				current.Plan.NextCandy = current.Plan.UpdatedAt.Add(time.Duration(in.CandySeconds) * time.Second)
			}
			if record.Plan.PelicanSeconds != in.PelicanSeconds {
				current.Plan.NextPelican = current.Plan.UpdatedAt.Add(time.Duration(in.PelicanSeconds) * time.Second)
			}
		}
		current.Plan.Revision++
		current.Secret = ciphertext
		st.Plans[id] = current
		result = current.Plan
		return nil
	})
	return result, err
}

func intelligenceVisibilityOnly(plan IntelligencePlan, in intelligencePlanInput) bool {
	// A plan with an unfinished native-key creation still follows the existing
	// reconciliation path. Never turn an execution/limit edit into metadata.
	return plan.KeyID > 0 && in.Visibility != nil && intelligencePlanVisibility(plan) != *in.Visibility &&
		plan.GroupID == in.GroupID && plan.Name == in.Name &&
		plan.Model == in.Model && plan.Effort == in.Effort && plan.Enabled == in.Enabled &&
		plan.CandySeconds == in.CandySeconds && plan.PelicanSeconds == in.PelicanSeconds &&
		plan.DailyLimit == in.DailyLimit && plan.Quota == in.Quota &&
		(in.CreativeEveryFive == nil || plan.CreativeEveryFive == *in.CreativeEveryFive)
}

func intelligencePauseOnly(plan IntelligencePlan, in intelligencePlanInput) bool {
	return plan.KeyID > 0 && plan.Enabled && !in.Enabled &&
		plan.GroupID == in.GroupID && plan.Name == in.Name &&
		plan.Model == in.Model && plan.Effort == in.Effort &&
		plan.CandySeconds == in.CandySeconds && plan.PelicanSeconds == in.PelicanSeconds &&
		plan.DailyLimit == in.DailyLimit && plan.Quota == in.Quota &&
		(in.Visibility == nil || plan.Visibility == *in.Visibility) &&
		(in.CreativeEveryFive == nil || plan.CreativeEveryFive == *in.CreativeEveryFive)
}

func (s *Server) intelligenceKey(r *http.Request, record intelligencePlanRecord, in intelligencePlanInput) (intelligenceNativeKey, error) {
	var key intelligenceNativeKey
	if record.Plan.KeyID > 0 {
		path := "/api/v1/keys/" + strconv.FormatInt(record.Plan.KeyID, 10)
		if err := s.mainAPI(r, "GET", path, nil, "", &key); err != nil {
			return key, err
		}
		if key.UserID != record.Plan.OwnerID || key.GroupID != record.Plan.GroupID {
			return key, ErrCredential
		}
		if key.Quota != in.Quota || key.DailyLimit != in.DailyLimit {
			// Existing native limits are authoritative; never reset usage or
			// replenish the wallet while adjusting a monitoring configuration.
			err := s.mainAPI(r, "PUT", path, map[string]any{"quota": in.Quota, "rate_limit_1d": in.DailyLimit}, "", &key)
			return key, err
		}
		return key, nil
	}
	name := "智商测试 · " + record.Operation
	var list struct {
		Items []intelligenceNativeKey `json:"items"`
	}
	err := s.mainAPI(r, "GET", "/api/v1/keys?search="+url.QueryEscape(record.Operation)+"&page_size=100", nil, "", &list)
	if err != nil {
		return key, err
	}
	for _, candidate := range list.Items {
		if candidate.Name == name && candidate.GroupID == record.Plan.GroupID {
			if key.ID != 0 {
				return key, ErrConflict
			}
			key = candidate
		}
	}
	if key.ID != 0 {
		return key, nil
	}
	if record.Attempted {
		return key, ErrIntelligencePending
	}
	err = s.Intelligence.Store.change(func(st *intelligenceState) error {
		current := st.Plans[record.Plan.ID]
		if current.Attempted || current.Plan.Revision != record.Plan.Revision {
			return ErrConflict
		}
		current.Attempted = true
		st.Plans[record.Plan.ID] = current
		return nil
	})
	if err != nil {
		return key, err
	}
	days := in.ExpiresDays
	if days == 0 {
		days = 30
	}
	err = s.mainAPI(r, "POST", "/api/v1/keys", map[string]any{"name": name, "group_id": in.GroupID, "quota": in.Quota, "rate_limit_1d": in.DailyLimit, "expires_in_days": days}, "intelligence-key-"+record.Operation, &key)
	return key, err
}

func intelligenceAPIError(w http.ResponseWriter, err error) {
	code, status := "intelligence_operation_failed", 503
	switch {
	case errors.Is(err, ErrIntelligenceInvalid):
		code, status = "invalid_settings", 400
	case errors.Is(err, ErrCredential):
		code, status = "monitor_key_unavailable", 403
	case errors.Is(err, ErrNotFound):
		code, status = "record_unavailable", 404
	case errors.Is(err, ErrConflict):
		code, status = "settings_changed", 409
	case errors.Is(err, ErrBusy):
		code, status = "test_in_progress", 409
	case errors.Is(err, ErrIntelligencePending):
		code, status = "key_creation_needs_review", 409
	}
	jsonResponse(w, status, map[string]string{"error": code})
}

func (s *Server) intelligenceReadAccess(w http.ResponseWriter, r *http.Request) (bool, bool) {
	user, ok := r.Context().Value(identityKey{}).(Identity)
	if !ok {
		jsonResponse(w, 401, map[string]string{"error": "admin_login_required"})
		return false, false
	}
	if user.Role == "admin" {
		return true, true
	}
	s.Intelligence.Store.mu.Lock()
	access := intelligenceViewerAccess(s.Intelligence.Store.state)
	s.Intelligence.Store.mu.Unlock()
	if access == "authenticated" {
		return true, false
	}
	jsonResponse(w, 403, map[string]string{"error": "admin_required"})
	return false, false
}

func (s *Server) intelligenceRoutes(mux *http.ServeMux) {
	if s.Intelligence == nil {
		return
	}
	mux.HandleFunc("GET /intelligence-tests", func(w http.ResponseWriter, r *http.Request) {
		if s.HomeFiles == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, s.HomeFiles, "intelligence.html")
	})
	snapshot := func(w http.ResponseWriter, r *http.Request) {
		allowed, admin := s.intelligenceReadAccess(w, r)
		if !allowed {
			return
		}
		access, plans, runs, err := s.Intelligence.Store.viewerSnapshot(admin)
		if err != nil {
			intelligenceAPIError(w, ErrResponse)
			return
		}
		jsonResponse(w, 200, map[string]any{"plans": plans, "runs": runs, "viewer_access": access, "admin_only": access == "admin", "can_manage": admin, "server_time": s.Intelligence.now()})
	}
	mux.HandleFunc("GET /public/intelligence-tests", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 401, map[string]string{"error": "admin_login_required"})
	})
	mux.HandleFunc("GET /api/intelligence-tests/snapshot", snapshot)
	result := func(w http.ResponseWriter, r *http.Request) {
		allowed, admin := s.intelligenceReadAccess(w, r)
		if !allowed {
			return
		}
		id := r.PathValue("id")
		view, result, err := s.Intelligence.Store.resultView(id, admin)
		if err != nil {
			intelligenceAPIError(w, ErrNotFound)
			return
		}
		jsonResponse(w, 200, map[string]any{"run": view, "answer": result.Answer, "html": result.HTML})
	}
	mux.HandleFunc("GET /public/intelligence-tests/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 401, map[string]string{"error": "admin_login_required"})
	})
	mux.HandleFunc("GET /api/intelligence-tests/runs/{id}", result)
	mux.HandleFunc("GET /api/intelligence-tests/settings", func(w http.ResponseWriter, r *http.Request) {
		groups, err := s.intelligenceGroups(r)
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		state := s.Intelligence.Store.snapshot()
		plans := s.Intelligence.settingsPlans(state)
		jsonResponse(w, 200, map[string]any{"revision": state.SettingsRevision, "admin_only": intelligenceViewerAccess(state) == "admin", "viewer_access": intelligenceViewerAccess(state), "plans": plans, "groups": groups})
	})
	mux.HandleFunc("GET /api/intelligence-tests/order", func(w http.ResponseWriter, r *http.Request) {
		order, err := s.Intelligence.Store.orderView()
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, order)
	})
	mux.HandleFunc("PUT /api/intelligence-tests/order", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision int64    `json:"revision"`
			PlanIDs  []string `json:"plan_ids"`
		}
		if decode(w, r, &in) != nil || in.Revision < 0 || in.PlanIDs == nil || len(in.PlanIDs) > 32 {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		if err := s.Intelligence.Store.reorder(in.Revision, in.PlanIDs); err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]int64{"revision": in.Revision + 1})
	})
	mux.HandleFunc("PUT /api/intelligence-tests/settings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision     int64  `json:"revision"`
			AdminOnly    *bool  `json:"admin_only"`
			ViewerAccess string `json:"viewer_access"`
		}
		if decode(w, r, &in) != nil || (in.AdminOnly != nil && !*in.AdminOnly) || (in.ViewerAccess != "admin" && in.ViewerAccess != "authenticated" && !(in.ViewerAccess == "" && in.AdminOnly != nil && *in.AdminOnly)) {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		err := s.Intelligence.Store.change(func(st *intelligenceState) error {
			if st.SettingsRevision != in.Revision {
				return ErrConflict
			}
			st.AdminOnly = true
			st.ViewerAccess = "admin"
			access := in.ViewerAccess
			if access == "" {
				access = "admin"
			}
			st.ViewerPolicy = &intelligenceViewerPolicy{Version: 1, Access: access}
			st.SettingsRevision++
			return nil
		})
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"admin_only": in.ViewerAccess != "authenticated", "viewer_access": intelligenceViewerAccess(s.Intelligence.Store.snapshot()), "revision": in.Revision + 1})
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var in intelligencePlanInput
		if decode(w, r, &in) != nil {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		plan, err := s.saveIntelligencePlan(r, r.PathValue("id"), in)
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, plan)
	}
	mux.HandleFunc("POST /api/intelligence-tests/plans", save)
	mux.HandleFunc("PUT /api/intelligence-tests/plans/{id}", save)
	mux.HandleFunc("DELETE /api/intelligence-tests/plans/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision int64 `json:"revision"`
		}
		if decode(w, r, &in) != nil {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		out, err := s.Intelligence.deletePlan(r.PathValue("id"), in.Revision, r.Context().Value(adminKey{}).(int64))
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, out)
	})
	mux.HandleFunc("POST /api/intelligence-tests/plans/{id}/clear-history", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision int64    `json:"revision"`
			RunIDs   []string `json:"run_ids"`
		}
		if decode(w, r, &in) != nil {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		out, err := s.Intelligence.clearHistory(r.PathValue("id"), in.Revision, in.RunIDs, r.Context().Value(adminKey{}).(int64))
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 200, out)
	})
	mux.HandleFunc("POST /api/intelligence-tests/plans/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Benchmark string `json:"benchmark"`
			RequestID string `json:"request_id"`
		}
		if decode(w, r, &in) != nil || len(in.RequestID) < 16 || len(in.RequestID) > 80 {
			intelligenceAPIError(w, ErrIntelligenceInvalid)
			return
		}
		run, err := s.Intelligence.Enqueue(r.PathValue("id"), in.Benchmark, in.RequestID, true)
		if err != nil {
			intelligenceAPIError(w, err)
			return
		}
		jsonResponse(w, 202, run)
	})
}
