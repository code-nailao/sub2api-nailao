package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// UpdateSortOrders locks targets in a stable order and commits the batch once.
// Missing/deleted targets and write failures leave every saved position intact.
func (r *channelMonitorRepository) UpdateSortOrders(ctx context.Context, updates []service.ChannelMonitorSortOrderUpdate) error {
	if err := service.ValidateChannelMonitorSortOrders(updates); err != nil {
		return err
	}
	ids := make([]int64, len(updates))
	args := make([]any, 0, len(updates)*2+1)
	clauses := make([]string, len(updates))
	for i, update := range updates {
		ids[i] = update.ID
		clauses[i] = fmt.Sprintf("WHEN $%d THEN $%d", i*2+1, i*2+2)
		args = append(args, update.ID, update.SortOrder)
	}
	args = append(args, pq.Array(ids))
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin monitor ordering: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT id FROM channel_monitors WHERE id = ANY($1) ORDER BY id FOR UPDATE", pq.Array(ids))
	if err != nil {
		return fmt.Errorf("lock monitor ordering: %w", err)
	}
	count := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		count++
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if count != len(ids) {
		return service.ErrChannelMonitorNotFound
	}
	query := fmt.Sprintf("UPDATE channel_monitors SET sort_order = CASE id %s ELSE sort_order END WHERE id = ANY($%d)", strings.Join(clauses, " "), len(args))
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("save monitor ordering: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != int64(len(ids)) {
		return service.ErrChannelMonitorNotFound
	}
	return tx.Commit()
}
