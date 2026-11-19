package portal

import (
	"os"
	"path/filepath"
	"time"
)

type intelligencePlanDeletion struct {
	ID        string `json:"id"`
	ArchiveID string `json:"archive_id"`
	KeyID     int64  `json:"key_id"`
}

type intelligencePlanArchive struct {
	At            time.Time              `json:"at"`
	Actor         int64                  `json:"actor_id"`
	Record        intelligencePlanRecord `json:"record"`
	PlanOrder     []string               `json:"plan_order"`
	OrderRevision int64                  `json:"order_revision"`
}

// Delete only the monitoring configuration. Results and paid-request tombstones
// keep their existing retention, and the native group and key remain untouched.
func (s *IntelligenceService) deletePlan(id string, revision, actor int64) (intelligencePlanDeletion, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if !intelligenceID.MatchString(id) || revision < 1 || actor < 1 {
		return intelligencePlanDeletion{}, ErrIntelligenceInvalid
	}
	var out intelligencePlanDeletion
	now := s.now()
	err := s.Store.change(func(st *intelligenceState) error {
		p, ok := st.Plans[id]
		if !ok {
			return ErrNotFound
		}
		if p.Plan.Revision != revision {
			return ErrConflict
		}
		if p.Plan.KeyID == 0 && p.Attempted {
			return ErrIntelligencePending
		}
		for _, row := range st.Runs {
			if row.Run.PlanID == id && intelligenceInFlight(row, now) {
				return ErrBusy
			}
		}
		archiveID := randomID()
		dir := filepath.Join(s.Store.dir, "plan-archives")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		archive := intelligencePlanArchive{now, actor, p, append([]string(nil), st.PlanOrder...), st.OrderRevision}
		// The private archive retains the encrypted credential and creator state
		// for recovery. An archive failure must abort before changing live state.
		if err := s.Store.writer(filepath.Join(dir, archiveID+".json"), archive); err != nil {
			return err
		}
		delete(st.Plans, id)
		order := make([]string, 0, len(st.PlanOrder))
		for _, planID := range st.PlanOrder {
			if planID != id {
				order = append(order, planID)
			}
		}
		st.PlanOrder = order
		st.OrderRevision++
		out = intelligencePlanDeletion{ID: id, ArchiveID: archiveID, KeyID: p.Plan.KeyID}
		return nil
	})
	if err != nil {
		return intelligencePlanDeletion{}, err
	}
	return out, nil
}
