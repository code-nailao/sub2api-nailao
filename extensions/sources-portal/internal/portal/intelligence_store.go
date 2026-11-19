package portal

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const intelligenceModel = "gpt-6-astra"

func intelligenceDrawing(benchmark string) bool {
	return benchmark == "pelican" || benchmark == "creative"
}
func intelligenceCategory(benchmark string) string {
	if intelligenceDrawing(benchmark) {
		return "drawing"
	}
	return benchmark
}
func intelligenceNextDrawing(plan IntelligencePlan) string {
	if plan.CreativeEveryFive && plan.DrawingSequence%5 == 4 {
		return "creative"
	}
	return "pelican"
}

var intelligenceID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var ErrIntelligenceInvalid = errors.New("invalid_intelligence_settings")
var ErrIntelligencePending = errors.New("intelligence_operation_pending")

type IntelligencePlan struct {
	Visibility        string     `json:"visibility"`
	CreativeEveryFive bool       `json:"creative_every_five"`
	DrawingSequence   uint64     `json:"drawing_sequence"`
	ID                string     `json:"id"`
	Revision          int64      `json:"revision"`
	GroupID           int64      `json:"group_id"`
	Name              string     `json:"name"`
	Model             string     `json:"model"`
	Effort            string     `json:"reasoning_effort"`
	Enabled           bool       `json:"enabled"`
	CandySeconds      int        `json:"candy_interval_seconds"`
	PelicanSeconds    int        `json:"pelican_interval_seconds"`
	NextCandy         time.Time  `json:"next_candy_at"`
	NextPelican       time.Time  `json:"next_pelican_at"`
	KeyID             int64      `json:"key_id"`
	OwnerID           int64      `json:"owner_id"`
	DailyLimit        float64    `json:"daily_limit"`
	Quota             float64    `json:"quota"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	KeyStatus         string     `json:"key_status"`
	UpdatedAt         time.Time  `json:"updated_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

func intelligenceKeyExpired(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && !expiresAt.After(now)
}

type IntelligenceRun struct {
	ID           string     `json:"id"`
	PlanID       string     `json:"plan_id"`
	Name         string     `json:"name"`
	Benchmark    string     `json:"benchmark"`
	Model        string     `json:"model"`
	Effort       string     `json:"reasoning_effort"`
	Status       string     `json:"status"`
	Phase        string     `json:"phase"`
	Verdict      string     `json:"verdict"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	DurationMS   *int64     `json:"duration_ms,omitempty"`
	InputTokens  *int64     `json:"input_tokens,omitempty"`
	OutputTokens *int64     `json:"output_tokens,omitempty"`
	HasResult    bool       `json:"has_result"`
	HasHTML      bool       `json:"has_html"`
	ErrorCode    string     `json:"error_code,omitempty"`
	Trigger      string     `json:"trigger"`
}

type intelligencePlanRecord struct {
	Plan      IntelligencePlan `json:"plan"`
	Secret    string           `json:"encrypted_key"`
	Attempted bool             `json:"key_create_attempted"`
	Operation string           `json:"key_operation"`
}

type intelligenceRunRecord struct {
	Run         IntelligenceRun `json:"run"`
	RemoteID    string          `json:"remote_id,omitempty"`
	NextPoll    time.Time       `json:"next_poll_at"`
	Deadline    time.Time       `json:"deadline"`
	RequestHash string          `json:"request_hash"`
}

type intelligenceRequest struct {
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"`
}

type intelligenceState struct {
	Version          int                               `json:"version"`
	Revision         int64                             `json:"revision"`
	AdminOnly        bool                              `json:"admin_only"`
	ViewerAccess     string                            `json:"viewer_access,omitempty"`
	ViewerPolicy     *intelligenceViewerPolicy         `json:"viewer_policy,omitempty"`
	SubmitAfter      time.Time                         `json:"submit_after"`
	SettingsRevision int64                             `json:"settings_revision"`
	OrderRevision    int64                             `json:"order_revision,omitempty"`
	PlanOrder        []string                          `json:"plan_order,omitempty"`
	Plans            map[string]intelligencePlanRecord `json:"plans"`
	Runs             map[string]intelligenceRunRecord  `json:"runs"`
	Requests         map[string]intelligenceRequest    `json:"requests"`
}

// Older readers ignore this policy and see the private legacy access fields.
type intelligenceViewerPolicy struct {
	Version int    `json:"version"`
	Access  string `json:"access"`
}

type IntelligenceResult struct {
	Answer string `json:"answer,omitempty"`
	HTML   string `json:"html,omitempty"`
}

// The existing Portal lifetime lock owns this store as well. No second writer,
// database or daemon is introduced.
type IntelligenceStore struct {
	mu     sync.Mutex
	dir    string
	path   string
	aead   cipher.AEAD
	state  intelligenceState
	failed error
	writer func(string, any) error
}

func OpenIntelligenceStore(owner *Store) (*IntelligenceStore, error) {
	if owner == nil || owner.lock == nil {
		return nil, errors.New("portal_store_owner_required")
	}
	dir := filepath.Join(filepath.Dir(owner.path), "intelligence")
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0700); err != nil {
		return nil, err
	}
	s := &IntelligenceStore{dir: dir, path: filepath.Join(dir, "state.json"), aead: owner.aead, writer: writeJSONAtomic}
	s.state = intelligenceState{Version: 1, AdminOnly: true, Plans: map[string]intelligencePlanRecord{}, Runs: map[string]intelligenceRunRecord{}, Requests: map[string]intelligenceRequest{}}
	raw, err := os.ReadFile(s.path)
	if err == nil {
		if json.Unmarshal(raw, &s.state) != nil || s.state.Version != 1 || s.state.Plans == nil || s.state.Runs == nil || s.state.Requests == nil {
			return nil, errors.New("invalid_intelligence_store")
		}
		for id, p := range s.state.Plans {
			if !intelligenceID.MatchString(id) || p.Plan.ID != id {
				return nil, errors.New("invalid_intelligence_plan")
			}
			if p.Secret != "" {
				if _, err := s.decrypt(p); err != nil {
					return nil, errors.New("intelligence_key_mismatch")
				}
			}
		}
		for id, r := range s.state.Runs {
			if !intelligenceID.MatchString(id) || r.Run.ID != id {
				return nil, errors.New("invalid_intelligence_run")
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.state.ViewerPolicy != nil && s.state.ViewerPolicy.Version != 1 {
		return nil, errors.New("invalid_intelligence_viewer_policy")
	}
	if s.state.ViewerPolicy == nil || !s.state.AdminOnly || s.state.ViewerAccess != "admin" {
		if err := s.change(func(st *intelligenceState) error {
			if st.ViewerPolicy == nil {
				access := "admin"
				if st.ViewerAccess == "authenticated" {
					access = "authenticated"
				}
				st.ViewerPolicy = &intelligenceViewerPolicy{Version: 1, Access: access}
				for id, row := range st.Plans {
					// Preserve shared042 plans; after a legacy writer drops unknown policy
					// fields, its private access keeps every plan private on the next upgrade.
					row.Plan.Visibility = access
					st.Plans[id] = row
				}
			}
			st.AdminOnly, st.ViewerAccess = true, "admin"
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func cloneIntelligenceState(v intelligenceState) intelligenceState {
	raw, _ := json.Marshal(v)
	var out intelligenceState
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *IntelligenceStore) snapshot() intelligenceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneIntelligenceState(s.state)
}

func (s *IntelligenceStore) healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed == nil
}

func (s *IntelligenceStore) change(fn func(*intelligenceState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return ErrPersistenceUncertain
	}
	next := cloneIntelligenceState(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	next.Revision++
	err := s.writer(s.path, next)
	if err == nil || writeCommitted(err) {
		s.state = next
	}
	if err != nil {
		s.failed = err
	}
	return err
}

func intelligenceAAD(p IntelligencePlan) []byte {
	return []byte(fmt.Sprintf("intelligence-key\n%s\n%d", p.ID, p.GroupID))
}

func (s *IntelligenceStore) encrypt(p IntelligencePlan, key string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(key), intelligenceAAD(p))), nil
}

func (s *IntelligenceStore) decrypt(p intelligencePlanRecord) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(p.Secret)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", ErrCredential
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], intelligenceAAD(p.Plan))
	return string(plain), err
}

func (s *IntelligenceStore) saveResult(id string, result IntelligenceResult) error {
	if !intelligenceID.MatchString(id) || len(result.HTML) > 2<<20 || len(result.Answer) > 512<<10 {
		return ErrIntelligenceInvalid
	}
	if !s.resultSafe(result) {
		return ErrUnsafeIntelligenceResult
	}
	return s.writer(filepath.Join(s.dir, "results", id+".json"), result)
}

func (s *IntelligenceStore) result(id string) (IntelligenceResult, error) {
	var result IntelligenceResult
	if !intelligenceID.MatchString(id) {
		return result, ErrNotFound
	}
	state := s.snapshot()
	r, ok := state.Runs[id]
	if !ok || !r.Run.HasResult {
		return result, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, "results", id+".json"))
	if err != nil || len(raw) > 16<<20 || json.Unmarshal(raw, &result) != nil {
		return result, ErrResponse
	}
	if !s.resultSafe(result) {
		return IntelligenceResult{}, ErrUnsafeIntelligenceResult
	}
	return result, nil
}

func intelligenceActive(status string) bool {
	return status == "queued" || status == "submitting" || status == "running"
}

func intelligenceInFlight(r intelligenceRunRecord, now time.Time) bool {
	return intelligenceActive(r.Run.Status) || r.Run.Status == "unknown" && r.Deadline.After(now)
}

func (s *IntelligenceStore) prune(now time.Time) error {
	var removed []string
	err := s.change(func(st *intelligenceState) error {
		buckets := map[string][]intelligenceRunRecord{}
		for _, r := range st.Runs {
			if !intelligenceInFlight(r, now) {
				key := r.Run.PlanID + ":" + intelligenceCategory(r.Run.Benchmark)
				buckets[key] = append(buckets[key], r)
			}
		}
		for _, rows := range buckets {
			sort.Slice(rows, func(i, j int) bool { return rows[i].Run.StartedAt.After(rows[j].Run.StartedAt) })
			keep := 60
			if intelligenceDrawing(rows[0].Run.Benchmark) {
				keep = 20
			}
			for i, r := range rows {
				if i >= keep || r.Run.StartedAt.Before(now.Add(-7*24*time.Hour)) {
					delete(st.Runs, r.Run.ID)
					removed = append(removed, r.Run.ID)
				}
			}
		}
		for key, r := range st.Requests {
			if r.At.Before(now.Add(-7 * 24 * time.Hour)) {
				delete(st.Requests, key)
			}
		}
		return nil
	})
	if err == nil {
		for _, id := range removed {
			_ = os.Remove(filepath.Join(s.dir, "results", id+".json"))
		}
	}
	return err
}
