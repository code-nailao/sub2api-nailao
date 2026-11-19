package portal

import "sort"

type intelligenceOrderPlan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type intelligenceOrderView struct {
	Revision int64                   `json:"revision"`
	Plans    []intelligenceOrderPlan `json:"plans"`
}

// Unranked plans retain the legacy order and follow the saved plans. New plans
// therefore appear at the end without changing existing scheduling metadata.
func orderedIntelligencePlans(st intelligenceState) []IntelligencePlan {
	positions := make(map[string]int, len(st.PlanOrder))
	for index, id := range st.PlanOrder {
		positions[id] = index
	}
	plans := make([]IntelligencePlan, 0, len(st.Plans))
	for _, row := range st.Plans {
		plans = append(plans, row.Plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		ai, aRanked := positions[a.ID]
		bi, bRanked := positions[b.ID]
		if aRanked != bRanked {
			return aRanked
		}
		if aRanked && ai != bi {
			return ai < bi
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.GroupID != b.GroupID {
			return a.GroupID < b.GroupID
		}
		return a.ID < b.ID
	})
	return plans
}

func (s *IntelligenceStore) orderView() (intelligenceOrderView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.secretsLocked()
	if err != nil {
		return intelligenceOrderView{}, err
	}
	out := intelligenceOrderView{Revision: s.state.OrderRevision, Plans: []intelligenceOrderPlan{}}
	for _, plan := range orderedIntelligencePlans(s.state) {
		out.Plans = append(out.Plans, intelligenceOrderPlan{ID: plan.ID, Name: intelligenceDisplayName(plan.Name, keys)})
	}
	return out, nil
}

func (s *IntelligenceStore) reorder(revision int64, ids []string) error {
	return s.change(func(st *intelligenceState) error {
		if revision != st.OrderRevision || len(ids) != len(st.Plans) {
			return ErrConflict
		}
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if _, exists := st.Plans[id]; !exists || seen[id] || !intelligenceID.MatchString(id) {
				return ErrIntelligenceInvalid
			}
			seen[id] = true
		}
		st.PlanOrder = append([]string(nil), ids...)
		st.OrderRevision++
		return nil
	})
}
