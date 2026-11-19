package portal

import (
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var ErrUnsafeIntelligenceResult = errors.New("result_unavailable")
var intelligenceResultPath = regexp.MustCompile(`^/api/intelligence-tests/runs/[a-f0-9]{32}$`)
var intelligenceCredential = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]{12,}|\bbearer\s+[a-z0-9._~+/-]{16,}|\beyJ[a-z0-9_-]{12,}\.[a-z0-9_-]{12,}\.[a-z0-9_-]{12,}`)

func intelligenceReadRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/api/intelligence-tests/snapshot" || intelligenceResultPath.MatchString(r.URL.Path))
}
func intelligenceViewerAccess(st intelligenceState) string {
	if st.ViewerPolicy != nil && st.ViewerPolicy.Version == 1 && st.ViewerPolicy.Access == "authenticated" {
		return "authenticated"
	}
	return "admin"
}
func intelligencePlanVisibility(p IntelligencePlan) string {
	if p.Visibility == "authenticated" {
		return "authenticated"
	}
	return "admin"
}
func intelligencePlanReadable(st intelligenceState, id string, admin bool) bool {
	p, ok := st.Plans[id]
	return ok && (admin || intelligenceViewerAccess(st) == "authenticated" && intelligencePlanVisibility(p.Plan) == "authenticated")
}
func intelligenceSafeText(value string, secrets []string) bool {
	for pass := 0; pass < 3; pass++ {
		if intelligenceCredential.MatchString(value) {
			return false
		}
		for _, secret := range secrets {
			for _, encoded := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), base64.RawURLEncoding.EncodeToString([]byte(secret))} {
				if encoded != "" && strings.Contains(value, encoded) {
					return false
				}
			}
		}
		value = html.UnescapeString(value)
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
	}
	return true
}
func (s *IntelligenceStore) secretsLocked() ([]string, error) {
	keys := []string{}
	for _, p := range s.state.Plans {
		if p.Secret == "" {
			continue
		}
		key, err := s.decrypt(p)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}
func (s *IntelligenceStore) resultSafe(result IntelligenceResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.secretsLocked()
	return err == nil && intelligenceSafeText(result.Answer, keys) && intelligenceSafeText(result.HTML, keys)
}
func intelligenceDisplayName(value string, keys []string) string {
	if !intelligenceSafeText(value, keys) {
		return "检测分组"
	}
	return value
}

// Explicit projection: adding private fields to stored records never widens this API.
func intelligenceRunView(r IntelligenceRun, keys []string) map[string]any {
	view := map[string]any{"id": r.ID, "plan_id": r.PlanID, "name": intelligenceDisplayName(r.Name, keys), "benchmark": r.Benchmark, "model": r.Model, "reasoning_effort": r.Effort, "status": r.Status, "phase": r.Phase, "verdict": r.Verdict, "started_at": r.StartedAt, "has_result": r.HasResult, "has_html": r.HasHTML, "trigger": r.Trigger}
	if r.FinishedAt != nil {
		view["finished_at"] = r.FinishedAt
	}
	if r.DurationMS != nil {
		view["duration_ms"] = r.DurationMS
	}
	if r.InputTokens != nil {
		view["input_tokens"] = r.InputTokens
	}
	if r.OutputTokens != nil {
		view["output_tokens"] = r.OutputTokens
	}
	if r.ErrorCode != "" {
		view["error_code"] = "test_failed"
	}
	return view
}
func (s *IntelligenceStore) viewerSnapshot(admin bool) (string, []map[string]any, []map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.secretsLocked()
	if err != nil {
		return "", nil, nil, err
	}
	records := orderedIntelligencePlans(s.state)
	plans := make([]map[string]any, 0, len(records))
	for _, p := range records {
		if !intelligencePlanReadable(s.state, p.ID, admin) {
			continue
		}
		plans = append(plans, map[string]any{"visibility": intelligencePlanVisibility(p), "id": p.ID, "name": intelligenceDisplayName(p.Name, keys), "model": p.Model, "reasoning_effort": p.Effort, "enabled": p.Enabled, "candy_interval_seconds": p.CandySeconds, "pelican_interval_seconds": p.PelicanSeconds, "next_candy_at": p.NextCandy, "next_pelican_at": p.NextPelican, "creative_every_five": p.CreativeEveryFive, "next_drawing_benchmark": intelligenceNextDrawing(p)})
	}
	stored := make([]IntelligenceRun, 0, len(s.state.Runs))
	for _, r := range s.state.Runs {
		if intelligencePlanReadable(s.state, r.Run.PlanID, admin) {
			stored = append(stored, r.Run)
		}
	}
	sort.Slice(stored, func(i, j int) bool {
		if stored[i].StartedAt.Equal(stored[j].StartedAt) {
			return stored[i].ID < stored[j].ID
		}
		return stored[i].StartedAt.Before(stored[j].StartedAt)
	})
	runs := make([]map[string]any, 0, len(stored))
	for _, r := range stored {
		runs = append(runs, intelligenceRunView(r, keys))
	}
	return intelligenceViewerAccess(s.state), plans, runs, nil
}
func (s *IntelligenceStore) resultView(id string, admin bool) (map[string]any, IntelligenceResult, error) {
	s.mu.Lock()
	before, exists := s.state.Runs[id]
	allowed := exists && intelligencePlanReadable(s.state, before.Run.PlanID, admin)
	s.mu.Unlock()
	if !allowed {
		return nil, IntelligenceResult{}, ErrNotFound
	}
	result, err := s.result(id)
	if err != nil {
		return nil, IntelligenceResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.state.Runs[id]
	if !ok || !intelligencePlanReadable(s.state, row.Run.PlanID, admin) {
		return nil, IntelligenceResult{}, ErrNotFound
	}
	keys, err := s.secretsLocked()
	if err != nil {
		return nil, IntelligenceResult{}, err
	}
	return intelligenceRunView(row.Run, keys), result, nil
}
