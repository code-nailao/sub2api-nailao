package repository

import (
	"context"
	"database/sql"
	"errors"

	"entgo.io/ent/dialect"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type keyRequestSpeedGroup struct {
	id          int64
	enabled     bool
	defaultTier string
}

func (r *apiKeyRepository) withRequestSpeedTx(ctx context.Context, fn func(context.Context, *dbent.Client) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}
	if tx, ok := r.sql.(*dbent.Tx); ok {
		return fn(dbent.NewTxContext(ctx, tx), tx.Client())
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		// Some existing repository callers pass a transactional Client directly;
		// its owner, rather than this nested operation, controls commit/rollback.
		return fn(ctx, r.client)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(dbent.NewTxContext(ctx, tx), tx.Client()); err != nil {
		return err
	}
	return tx.Commit()
}

func nullableGroupIDEqual(stored sql.NullInt64, expected *int64) bool {
	return expected == nil && !stored.Valid || expected != nil && stored.Valid && stored.Int64 == *expected
}

// rowLockClause returns a Postgres row-lock suffix such as " FOR SHARE". SQLite
// (used only by the repository unit harness) has no row locks and rejects the
// syntax; its write transactions already lock the whole database, so omitting
// the clause there loses nothing. Executors without a known dialect are treated
// as Postgres, the only production database.
func rowLockClause(exec sqlExecutor, clause string) string {
	if d, ok := exec.(interface{ Driver() dialect.Driver }); ok && d.Driver().Dialect() == dialect.SQLite {
		return ""
	}
	return " " + clause
}

// The shared group lock makes a new Fast preference and disabling its group
// serialize. The group update validates existing Fast keys while holding UPDATE.
func loadKeyRequestSpeedGroup(ctx context.Context, exec sqlExecutor, groupID *int64) (*keyRequestSpeedGroup, error) {
	if groupID == nil {
		return nil, nil
	}
	rows, err := exec.QueryContext(ctx, `SELECT id, platform, request_speed_selection_enabled, default_request_speed_tier
		FROM groups WHERE id=$1 AND deleted_at IS NULL`+rowLockClause(exec, "FOR SHARE"), *groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrGroupNotFound
	}
	var out keyRequestSpeedGroup
	var platform string
	if err := rows.Scan(&out.id, &platform, &out.enabled, &out.defaultTier); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.enabled = out.enabled && platform == service.PlatformOpenAI
	return &out, nil
}

func resolveKeyRequestSpeedForWrite(ctx context.Context, exec sqlExecutor, groupID *int64, tier *string, useDefault bool) (*string, error) {
	group, err := loadKeyRequestSpeedGroup(ctx, exec, groupID)
	if err != nil {
		return nil, err
	}
	return validateKeyRequestSpeedForWrite(ctx, exec, group, tier, useDefault)
}

func validateKeyRequestSpeedForWrite(ctx context.Context, exec sqlExecutor, group *keyRequestSpeedGroup, tier *string, useDefault bool) (*string, error) {
	if group == nil || !group.enabled {
		if tier != nil && !useDefault {
			return nil, service.ErrRequestSpeedDisabled
		}
		return nil, nil
	}
	selected := "ultrafast"
	if useDefault {
		selected = group.defaultTier
	} else if tier != nil {
		selected = *tier
	}
	if !service.ValidRequestSpeedTier(selected) {
		return nil, service.ErrRequestSpeedInvalid
	}
	rows, err := exec.QueryContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM account_groups ag JOIN accounts a ON a.id=ag.account_id
		WHERE ag.group_id=$1 AND ag.request_speed_tiers ? $2
		AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='apikey'
		AND a.status='active' AND a.schedulable=true
	)`, group.id, selected)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrRequestSpeedUnavailable
	}
	var available bool
	if err := rows.Scan(&available); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !available {
		return nil, service.ErrRequestSpeedUnavailable
	}
	// Existing unassigned keys retain null during same-group edits. They resolve
	// to Ultrafast at request time, independent of future new-key defaults.
	if tier == nil && !useDefault {
		return nil, nil
	}
	return &selected, nil
}
