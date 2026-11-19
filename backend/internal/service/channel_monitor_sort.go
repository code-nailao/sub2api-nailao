package service

import "context"

type ChannelMonitorSortOrderUpdate struct {
	ID        int64 `json:"id"`
	SortOrder int   `json:"sort_order"`
}

// ValidateChannelMonitorSortOrders bounds the batch and rejects ambiguous IDs
// before any write. The same validation protects direct repository callers.
func ValidateChannelMonitorSortOrders(updates []ChannelMonitorSortOrderUpdate) error {
	if len(updates) == 0 || len(updates) > 10000 {
		return ErrChannelMonitorInvalidRequestBody
	}
	seen := make(map[int64]bool, len(updates))
	for _, update := range updates {
		if update.ID <= 0 || seen[update.ID] || update.SortOrder < 0 || update.SortOrder > 2147483647 {
			return ErrChannelMonitorInvalidRequestBody
		}
		seen[update.ID] = true
	}
	return nil
}

func (s *ChannelMonitorService) UpdateSortOrders(ctx context.Context, updates []ChannelMonitorSortOrderUpdate) error {
	if err := ValidateChannelMonitorSortOrders(updates); err != nil {
		return err
	}
	return s.repo.UpdateSortOrders(ctx, updates)
}
