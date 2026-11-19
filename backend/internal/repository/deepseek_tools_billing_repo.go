package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.DeepSeekUsageJournal = (*usageBillingRepository)(nil)

func (r *usageBillingRepository) BeginDeepSeekUsage(ctx context.Context, requestID string, keyID, userID, accountID int64, deadline time.Time) error {
	if !strings.HasPrefix(requestID, "deepseek-tools:") || keyID <= 0 || userID <= 0 || accountID <= 0 {
		return service.ErrUsageBillingRequestConflict
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO deepseek_usage_journal (request_id, api_key_id, user_id, account_id, deadline) VALUES ($1,$2,$3,$4,$5)`, requestID, keyID, userID, accountID, deadline)
	return err
}

func (r *usageBillingRepository) SaveDeepSeekUsage(ctx context.Context, checkpoint *service.DeepSeekUsageCheckpoint) error {
	if checkpoint == nil || checkpoint.Command == nil || checkpoint.Usage == nil || checkpoint.Revision <= 0 {
		return service.ErrUsageBillingRequestConflict
	}
	cmd, usage := checkpoint.Command, checkpoint.Usage
	cmd.Normalize()
	if cmd.RequestID != usage.RequestID || cmd.APIKeyID != usage.APIKeyID || cmd.UserID != usage.UserID || cmd.AccountID != usage.AccountID || cmd.DeepSeekToolFingerprint == "" {
		return service.ErrUsageBillingRequestConflict
	}
	command, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	row, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state, fingerprint string
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT state, revision, COALESCE(command->>'RequestFingerprint','') FROM deepseek_usage_journal WHERE request_id=$1 AND api_key_id=$2 AND user_id=$3 AND account_id=$4 FOR UPDATE`, cmd.RequestID, cmd.APIKeyID, cmd.UserID, cmd.AccountID).Scan(&state, &revision, &fingerprint)
	if err != nil {
		return err
	}
	if state != "pending" || checkpoint.Revision <= revision {
		if fingerprint == cmd.RequestFingerprint {
			return nil
		}
		return service.ErrUsageBillingRequestConflict
	}
	state = "pending"
	if checkpoint.Ready {
		state = "ready"
	}
	_, err = tx.ExecContext(ctx, `UPDATE deepseek_usage_journal SET command=$3, usage_log=$4, revision=$5, state=$6, updated_at=NOW() WHERE request_id=$1 AND api_key_id=$2`, cmd.RequestID, cmd.APIKeyID, command, row, checkpoint.Revision, state)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// The ordinary ledger claim, all financial effects, the one customer usage row,
// and the journal transition commit together. An ambiguous commit is safe to
// retry; model/tool operations are never part of recovery.
func (r *usageBillingRepository) SettleDeepSeekUsage(ctx context.Context, requestID string, keyID int64) (*service.DeepSeekUsageSettlement, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var deadline time.Time
	var command, row []byte
	err = tx.QueryRowContext(ctx, `SELECT state, deadline, command, usage_log FROM deepseek_usage_journal WHERE request_id=$1 AND api_key_id=$2 FOR UPDATE`, requestID, keyID).Scan(&state, &deadline, &command, &row)
	if err != nil {
		return nil, err
	}
	if state == "pending" && time.Now().Before(deadline) {
		return nil, nil
	}
	out := &service.DeepSeekUsageSettlement{Result: &service.UsageBillingApplyResult{}}
	if len(command) > 0 {
		out.Command, out.Usage = &service.UsageBillingCommand{}, &service.UsageLog{}
		if err = json.Unmarshal(command, out.Command); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(row, out.Usage); err != nil {
			return nil, err
		}
		if state == "settled" {
			return out, nil
		}
		applied, err := r.claimUsageBillingKey(ctx, tx, out.Command)
		if err != nil {
			return nil, err
		}
		out.Result.Applied = applied
		if applied {
			if err = r.applyUsageBillingEffects(ctx, tx, out.Command, out.Result); err != nil {
				return nil, err
			}
		}
		if err = execUsageLogInsertNoResult(ctx, tx, prepareUsageLogInsert(out.Usage)); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deepseek_usage_journal SET state='settled', updated_at=NOW() WHERE request_id=$1 AND api_key_id=$2`, requestID, keyID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *usageBillingRepository) ListPendingDeepSeekUsage(ctx context.Context, limit int) ([]service.DeepSeekUsageIdentity, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT request_id, api_key_id FROM deepseek_usage_journal WHERE state='ready' OR (state='pending' AND deadline<=NOW()) ORDER BY updated_at, request_id LIMIT $1`, min(max(limit, 1), 100))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.DeepSeekUsageIdentity
	for rows.Next() {
		var id service.DeepSeekUsageIdentity
		if err = rows.Scan(&id.RequestID, &id.APIKeyID); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Assert the standard SQL transaction provides the shared usage insertion seam.
var _ sqlExecutor = (*sql.Tx)(nil)
