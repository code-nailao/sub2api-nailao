package portal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type intelligenceHistoryClear struct {
	Revision      int64    `json:"revision"`
	RemovedIDs    []string `json:"removed_ids"`
	SkippedActive int      `json:"skipped_active"`
	ArchiveID     string   `json:"archive_id,omitempty"`
	PendingFiles  int      `json:"pending_files"`
}

// Clear an explicit reviewed selection. The existing owner serializes changes;
// active/unknown work and paid-request tombstones must never be removed.
func (s *IntelligenceService) clearHistory(planID string, revision int64, ids []string, actor int64) (intelligenceHistoryClear, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	out := intelligenceHistoryClear{RemovedIDs: []string{}}
	if !intelligenceID.MatchString(planID) || revision < 1 || actor < 1 || len(ids) == 0 || len(ids) > 256 {
		return out, ErrIntelligenceInvalid
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if !intelligenceID.MatchString(id) || selected[id] {
			return out, ErrIntelligenceInvalid
		}
		selected[id] = true
	}
	now := s.now()
	err := s.Store.change(func(st *intelligenceState) error {
		p, ok := st.Plans[planID]
		if !ok {
			return ErrNotFound
		}
		if p.Plan.Revision != revision {
			return ErrConflict
		}
		archive := struct {
			At       time.Time                        `json:"at"`
			Actor    int64                            `json:"actor_id"`
			Plan     string                           `json:"plan_id"`
			Revision int64                            `json:"revision"`
			Runs     map[string]intelligenceRunRecord `json:"runs"`
			Results  map[string]json.RawMessage       `json:"results"`
		}{now, actor, planID, revision, map[string]intelligenceRunRecord{}, map[string]json.RawMessage{}}
		for id := range selected {
			row, exists := st.Runs[id]
			if !exists {
				continue
			}
			if row.Run.PlanID != planID {
				return ErrIntelligenceInvalid
			}
			if intelligenceInFlight(row, now) {
				out.SkippedActive++
				continue
			}
			if row.Run.Status != "succeeded" && row.Run.Status != "failed" && row.Run.Status != "unknown" {
				return ErrIntelligenceInvalid
			}
			archive.Runs[id] = row
			if row.Run.HasResult {
				raw, err := os.ReadFile(filepath.Join(s.Store.dir, "results", id+".json"))
				if err != nil || len(raw) > 3<<20 || !json.Valid(raw) {
					return ErrResponse
				}
				archive.Results[id] = raw
			}
			out.RemovedIDs = append(out.RemovedIDs, id)
		}
		out.Revision = p.Plan.Revision
		if len(out.RemovedIDs) == 0 {
			return nil
		}
		sort.Strings(out.RemovedIDs)
		out.ArchiveID = randomID()
		dir := filepath.Join(s.Store.dir, "history-archives")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		// Backup failure aborts before state changes; archives are never HTTP-served.
		if err := writeJSONAtomic(filepath.Join(dir, out.ArchiveID+".json"), archive); err != nil {
			return err
		}
		for _, id := range out.RemovedIDs {
			delete(st.Runs, id)
		}
		p.Plan.Revision++
		st.Plans[planID] = p
		out.Revision = p.Plan.Revision
		return nil
	})
	if err != nil {
		return intelligenceHistoryClear{}, err
	}
	for _, id := range out.RemovedIDs {
		if e := os.Remove(filepath.Join(s.Store.dir, "results", id+".json")); e != nil && !os.IsNotExist(e) {
			out.PendingFiles++
		}
	}
	return out, nil
}
