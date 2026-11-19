package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.UpstreamRejectionBillingRepository = (*usageBillingRepository)(nil)
var _ service.UpstreamRejectionDueSource = (*usageBillingRepository)(nil)

const rejectionSelect = `SELECT snapshot, state, evidence, task_metadata, normal_billing_request_id,
	created_at, updated_at, sent_at, usage_projected_at, fingerprint, finalization_fingerprint,
	request_id, user_id, api_key_id, account_id, group_id, price, task_id, release_after
	FROM upstream_rejection_reservations`

type rejectionRow struct {
	service.UpstreamRejectionReservation
	fingerprint      string
	finalFingerprint string
}

type rejectionScanner interface{ Scan(...any) error }

func scanRejection(row rejectionScanner) (*rejectionRow, error) {
	r := &rejectionRow{}
	var snapshot, evidence, task []byte
	var requestID string
	var userID, keyID, accountID, groupID int64
	var price float64
	var taskID sql.NullString
	if err := row.Scan(&snapshot, &r.State, &evidence, &task, &r.NormalBillingRequestID, &r.CreatedAt, &r.UpdatedAt, &r.SentAt, &r.UsageProjectedAt, &r.fingerprint, &r.finalFingerprint,
		&requestID, &userID, &keyID, &accountID, &groupID, &price, &taskID, &r.ReleaseAfter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrUpstreamRejectionNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(snapshot, &r.UpstreamRejectionReserveCommand); err != nil {
		return nil, err
	}
	// A hold belongs to this exact immutable snapshot. Do not use another
	// reservation's or historical frozen money if durable identity drifts.
	if r.RequestID != requestID || r.UserID != userID || r.APIKeyID != keyID || r.AccountID != accountID || r.GroupID != groupID || r.Price != price || r.Fingerprint() != r.fingerprint {
		return nil, service.ErrUpstreamRejectionIntegrity
	}
	if err := json.Unmarshal(evidence, &r.Evidence); err != nil {
		return nil, err
	}
	if len(task) > 0 {
		if err := json.Unmarshal(task, &r.Task); err != nil {
			return nil, err
		}
	}
	if taskID.Valid != (r.Task != nil) || r.Task != nil && (r.Task.RequestID != requestID || r.Task.APIKeyID != keyID || r.Task.TaskID != taskID.String) {
		return nil, service.ErrUpstreamRejectionIntegrity
	}
	return r, nil
}

func (r *usageBillingRepository) rejectionDB() error {
	if r == nil || r.db == nil {
		return errors.New("upstream rejection billing database unavailable")
	}
	return nil
}

func (r *usageBillingRepository) GetUpstreamRejection(ctx context.Context, requestID string, apiKeyID int64) (*service.UpstreamRejectionReservation, error) {
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	if !service.IsUpstreamRejectionRequestID(requestID) || apiKeyID <= 0 {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	row, err := scanRejection(r.db.QueryRowContext(ctx, rejectionSelect+` WHERE request_id=$1 AND api_key_id=$2`, requestID, apiKeyID))
	if err != nil {
		return nil, err
	}
	return &row.UpstreamRejectionReservation, nil
}

// ListPending includes never-sent reserved intents for guarded recovery. The
// caller must persist MarkSent before HTTP; sent/unknown never expire for free.
func (r *usageBillingRepository) ListPendingUpstreamRejections(ctx context.Context, limit int) ([]service.UpstreamRejectionReservation, error) {
	page, err := r.ListPendingUpstreamRejectionsPage(ctx, &service.UpstreamRejectionPendingQuery{Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// Reconciliation must walk pages; long-lived unknown requests must not hide
// newer operations forever. The finalization APIs remain idempotent if a row
// moves during a scan and is seen again in a later page.
func (r *usageBillingRepository) ListPendingUpstreamRejectionsPage(ctx context.Context, query *service.UpstreamRejectionPendingQuery) (*service.UpstreamRejectionPendingPage, error) {
	return r.listUpstreamRejectionPage(ctx, query, false)
}

func (r *usageBillingRepository) ListUnprojectedUpstreamRejectionsPage(ctx context.Context, query *service.UpstreamRejectionPendingQuery) (*service.UpstreamRejectionPendingPage, error) {
	return r.listUpstreamRejectionPage(ctx, query, true)
}

func (r *usageBillingRepository) listUpstreamRejectionPage(ctx context.Context, query *service.UpstreamRejectionPendingQuery, projections bool) (*service.UpstreamRejectionPendingPage, error) {
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	if query == nil || query.Limit <= 0 || query.Limit > 1000 || query.After != nil && (query.After.UpdatedAt.IsZero() || !service.IsUpstreamRejectionRequestID(query.After.RequestID)) {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	where := ` WHERE state IN ('reserved','sent','unknown')`
	if projections {
		where = ` WHERE state='rejected' AND usage_projected_at IS NULL`
	}
	args := []any{query.Limit}
	if query.TasksOnly && !projections {
		where += ` AND task_id IS NOT NULL`
	}
	if query.After != nil {
		where += ` AND (updated_at,request_id)>($2,$3)`
		args = append(args, query.After.UpdatedAt, query.After.RequestID)
	}
	rows, err := r.db.QueryContext(ctx, rejectionSelect+where+` ORDER BY updated_at, request_id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]service.UpstreamRejectionReservation, 0)
	for rows.Next() {
		row, err := scanRejection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row.UpstreamRejectionReservation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &service.UpstreamRejectionPendingPage{Items: result}
	if len(result) == query.Limit {
		last := result[len(result)-1]
		page.Next = &service.UpstreamRejectionPendingCursor{UpdatedAt: last.UpdatedAt, RequestID: last.RequestID}
	}
	return page, nil
}

// ListDue returns only synchronous holds created with a deadline. Rows with a
// NULL deadline (historical rows and task-bound rows) are never listed, so the
// timer cannot sweep money that is reconciled by hand or settled by a task.
func (r *usageBillingRepository) ListDueUpstreamRejectionsPage(ctx context.Context, query *service.UpstreamRejectionDueQuery) (*service.UpstreamRejectionDuePage, error) {
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	if query == nil || query.Limit <= 0 || query.Limit > 1000 || query.After != nil && (query.After.ReleaseAfter.IsZero() || !service.IsUpstreamRejectionRequestID(query.After.RequestID)) {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	where := ` WHERE state IN ('sent','unknown') AND task_id IS NULL AND release_after IS NOT NULL AND release_after<=NOW()`
	args := []any{query.Limit}
	if query.After != nil {
		where += ` AND (release_after,request_id)>($2,$3)`
		args = append(args, query.After.ReleaseAfter, query.After.RequestID)
	}
	rows, err := r.db.QueryContext(ctx, rejectionSelect+where+` ORDER BY release_after, request_id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]service.UpstreamRejectionReservation, 0)
	for rows.Next() {
		row, err := scanRejection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row.UpstreamRejectionReservation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &service.UpstreamRejectionDuePage{Items: result}
	if len(result) == query.Limit && result[len(result)-1].ReleaseAfter != nil {
		last := result[len(result)-1]
		page.Next = &service.UpstreamRejectionDueCursor{ReleaseAfter: *last.ReleaseAfter, RequestID: last.RequestID}
	}
	return page, nil
}

// A successful usage insert and this acknowledgement may be separated by a
// crash. The rejected receipt remains discoverable until an equivalent durable
// user-facing charge row exists; repeating acknowledgement never charges money.
func (r *usageBillingRepository) MarkUpstreamRejectionUsageProjected(ctx context.Context, requestID string, keyID int64) error {
	if !service.IsUpstreamRejectionRequestID(requestID) || keyID <= 0 {
		return service.ErrUpstreamRejectionInvalid
	}
	if err := r.rejectionDB(); err != nil {
		return err
	}
	record, err := r.GetUpstreamRejection(ctx, requestID, keyID)
	if err != nil {
		return err
	}
	if record.State != service.UpstreamRejectionRejected {
		return service.ErrUpstreamRejectionState
	}
	if record.UsageProjectedAt != nil {
		return nil
	}
	var acknowledged string
	err = r.db.QueryRowContext(ctx, `UPDATE upstream_rejection_reservations r SET usage_projected_at=NOW()
		WHERE request_id=$1 AND api_key_id=$2 AND state='rejected' AND usage_projected_at IS NULL
		AND EXISTS(SELECT 1 FROM usage_logs u WHERE u.request_id=r.request_id AND u.api_key_id=r.api_key_id
		AND u.user_id=r.user_id AND u.account_id=r.account_id AND u.group_id=r.group_id
		AND u.actual_cost=r.price AND u.total_cost=r.price
		AND u.billing_snapshot->>'charge_kind'='upstream_rejection') RETURNING request_id`, requestID, keyID).Scan(&acknowledged)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record, err = r.GetUpstreamRejection(ctx, requestID, keyID)
	if err != nil {
		return err
	}
	if record.State != service.UpstreamRejectionRejected {
		return service.ErrUpstreamRejectionState
	}
	if record.UsageProjectedAt != nil {
		return nil
	}
	return service.ErrUpstreamRejectionIntegrity
}

// Lock order is user -> API key -> reservation in every financial mutation.
// Ordinary billing already updates user before key. No Redis state is consulted.
func lockRejectionOwner(ctx context.Context, tx *sql.Tx, userID, apiKeyID int64, admission bool) (float64, float64, error) {
	var balance, frozen float64
	var userActive, keyActive bool
	if err := tx.QueryRowContext(ctx, `SELECT balance, COALESCE(frozen_balance,0), status='active' AND deleted_at IS NULL FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&balance, &frozen, &userActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, service.ErrUserNotFound
		}
		return 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status='active' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at>NOW()) FROM api_keys WHERE id=$1 AND user_id=$2 FOR UPDATE`, apiKeyID, userID).Scan(&keyActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, service.ErrAPIKeyNotFound
		}
		return 0, 0, err
	}
	if admission && (!userActive || !keyActive) {
		return 0, 0, service.ErrUpstreamRejectionState
	}
	return balance, frozen, nil
}

func rejectionKeyAdmission(ctx context.Context, tx *sql.Tx, keyID, groupID int64, price float64) error {
	// Active holds count in every current window, even if an old hold crossed a
	// window boundary. It may still be captured now; silently expiring it would
	// let concurrent unknown requests exceed the new window's budget.
	var ownsGroup, quotaOK, h5OK, d1OK, d7OK bool
	err := tx.QueryRowContext(ctx, `WITH pending AS (
		SELECT COALESCE(SUM(price),0) AS amount FROM upstream_rejection_reservations
		WHERE api_key_id=$1 AND state IN ('reserved','sent','unknown')
	) SELECT COALESCE(group_id=$2,false),
		quota<=0 OR quota_used+pending.amount+$3<=quota,
		rate_limit_5h<=0 OR (CASE WHEN window_5h_start IS NULL OR window_5h_start+INTERVAL '5 hours'<=NOW() THEN 0 ELSE usage_5h END)+pending.amount+$3<=rate_limit_5h,
		rate_limit_1d<=0 OR (CASE WHEN window_1d_start IS NULL OR window_1d_start+INTERVAL '24 hours'<=NOW() THEN 0 ELSE usage_1d END)+pending.amount+$3<=rate_limit_1d,
		rate_limit_7d<=0 OR (CASE WHEN window_7d_start IS NULL OR window_7d_start+INTERVAL '7 days'<=NOW() THEN 0 ELSE usage_7d END)+pending.amount+$3<=rate_limit_7d
		FROM api_keys CROSS JOIN pending WHERE id=$1`, keyID, groupID, price).Scan(&ownsGroup, &quotaOK, &h5OK, &d1OK, &d7OK)
	if err != nil {
		return err
	}
	if !ownsGroup {
		return service.ErrUpstreamRejectionConflict
	}
	if !quotaOK {
		return service.ErrAPIKeyQuotaExhausted
	}
	if !h5OK {
		return service.ErrAPIKeyRateLimit5hExceeded
	}
	if !d1OK {
		return service.ErrAPIKeyRateLimit1dExceeded
	}
	if !d7OK {
		return service.ErrAPIKeyRateLimit7dExceeded
	}
	return nil
}

func rejectionEvent(ctx context.Context, tx *sql.Tx, row *rejectionRow, kind string, amount float64) error {
	snapshot, err := json.Marshal(&row.UpstreamRejectionReservation)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(row.Evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO upstream_rejection_billing_events(request_id,kind,api_key_id,user_id,amount,snapshot,evidence) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, row.RequestID, kind, row.APIKeyID, row.UserID, amount, string(snapshot), string(evidence))
	return err
}

func (r *usageBillingRepository) ReserveUpstreamRejection(ctx context.Context, cmd *service.UpstreamRejectionReserveCommand) (*service.UpstreamRejectionBillingResult, error) {
	// JSONB canonicalizes negative zero. Keep the write-once digest canonical as
	// well when a free supplier cost was represented by float negative zero.
	if cmd != nil && cmd.UpstreamCost == 0 {
		copy := *cmd
		copy.UpstreamCost = 0
		cmd = &copy
	}
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	balance, frozen, err := lockRejectionOwner(ctx, tx, cmd.UserID, cmd.APIKeyID, false)
	if err != nil {
		return nil, err
	}
	existing, err := scanRejection(tx.QueryRowContext(ctx, rejectionSelect+` WHERE request_id=$1 FOR UPDATE`, cmd.RequestID))
	if err == nil {
		if existing.fingerprint != cmd.Fingerprint() {
			return nil, service.ErrUpstreamRejectionConflict
		}
		return &service.UpstreamRejectionBillingResult{Reservation: &existing.UpstreamRejectionReservation, NewBalance: balance, FrozenBalance: frozen}, nil
	}
	if !errors.Is(err, service.ErrUpstreamRejectionNotFound) {
		return nil, err
	}
	if _, _, err := lockRejectionOwner(ctx, tx, cmd.UserID, cmd.APIKeyID, true); err != nil {
		return nil, err
	}
	// V1 does not mix this fixed-fee ledger with the Redis absolute-snapshot
	// platform quota flusher. Refuse before reserving or sending rather than
	// silently bypassing configured platform limits. This is an admission-time
	// decision; a later quota edit does not rewrite an already accepted bill.
	var platformQuotaConfigured bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_platform_quotas
		WHERE user_id=$1 AND platform='grok' AND deleted_at IS NULL AND
		(daily_limit_usd IS NOT NULL OR weekly_limit_usd IS NOT NULL OR monthly_limit_usd IS NOT NULL))`, cmd.UserID).Scan(&platformQuotaConfigured); err != nil {
		return nil, err
	}
	if platformQuotaConfigured {
		return nil, service.ErrUpstreamRejectionPlatformQuotaUnsupported
	}
	if err := rejectionKeyAdmission(ctx, tx, cmd.APIKeyID, cmd.GroupID, cmd.Price); err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO upstream_rejection_reservations(request_id,user_id,api_key_id,account_id,group_id,price,fingerprint,snapshot,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,'reserved')`, cmd.RequestID, cmd.UserID, cmd.APIKeyID, cmd.AccountID, cmd.GroupID, cmd.Price, cmd.Fingerprint(), string(snapshot))
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE users SET balance=balance-$1,frozen_balance=COALESCE(frozen_balance,0)+$1,updated_at=NOW() WHERE id=$2 AND balance>=$1 RETURNING balance,frozen_balance`, cmd.Price, cmd.UserID).Scan(&balance, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrInsufficientBalance
	}
	if err != nil {
		return nil, err
	}
	row, err := scanRejection(tx.QueryRowContext(ctx, rejectionSelect+` WHERE request_id=$1`, cmd.RequestID))
	if err != nil {
		return nil, err
	}
	if err := rejectionEvent(ctx, tx, row, service.UpstreamRejectionReserved, cmd.Price); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &service.UpstreamRejectionBillingResult{Applied: true, Reservation: &row.UpstreamRejectionReservation, NewBalance: balance, FrozenBalance: frozen}, nil
}

func (r *usageBillingRepository) lockedRejection(ctx context.Context, tx *sql.Tx, requestID string, keyID int64) (*rejectionRow, float64, float64, error) {
	// The first read is only to discover the immutable owner; reload after locks.
	row, err := scanRejection(tx.QueryRowContext(ctx, rejectionSelect+` WHERE request_id=$1 AND api_key_id=$2`, requestID, keyID))
	if err != nil {
		return nil, 0, 0, err
	}
	balance, frozen, err := lockRejectionOwner(ctx, tx, row.UserID, keyID, false)
	if err != nil {
		return nil, 0, 0, err
	}
	row, err = scanRejection(tx.QueryRowContext(ctx, rejectionSelect+` WHERE request_id=$1 AND api_key_id=$2 FOR UPDATE`, requestID, keyID))
	return row, balance, frozen, err
}

func (r *usageBillingRepository) MarkUpstreamRejectionSent(ctx context.Context, requestID string, keyID int64) (*service.UpstreamRejectionBillingResult, error) {
	if !service.IsUpstreamRejectionRequestID(requestID) || keyID <= 0 {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, balance, frozen, err := r.lockedRejection(ctx, tx, requestID, keyID)
	if err != nil {
		return nil, err
	}
	result := &service.UpstreamRejectionBillingResult{Reservation: &row.UpstreamRejectionReservation, NewBalance: balance, FrozenBalance: frozen}
	if row.State == service.UpstreamRejectionSent || row.State == service.UpstreamRejectionUnknown {
		return result, nil
	}
	if row.State != service.UpstreamRejectionReserved {
		return nil, service.ErrUpstreamRejectionState
	}
	// The deadline covers the longest request the policy allows plus the grace
	// period, measured on the database clock like every later comparison.
	err = tx.QueryRowContext(ctx, `UPDATE upstream_rejection_reservations SET state='sent',sent_at=NOW(),updated_at=NOW(),
		release_after=NOW()+make_interval(secs=>$2::double precision) WHERE request_id=$1 RETURNING sent_at,updated_at,release_after`,
		requestID, (service.UpstreamRejectionMaxRequestLifetime+service.UpstreamRejectionReleaseGrace).Seconds()).Scan(&row.SentAt, &row.UpdatedAt, &row.ReleaseAfter)
	if err != nil {
		return nil, err
	}
	row.State = service.UpstreamRejectionSent
	if err := rejectionEvent(ctx, tx, row, row.State, 0); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result.Applied = true
	return result, nil
}

func captureRejectionKey(ctx context.Context, tx *sql.Tx, keyID int64, amount float64) (bool, error) {
	// New fixed-fee path only: nil windows are expired, matching admission. Keep
	// legacy ordinary increment semantics unchanged. Soft-deleted keys still own
	// their durable bills and retain the associated counters for audit.
	var exhausted bool
	err := tx.QueryRowContext(ctx, `UPDATE api_keys SET
		quota_used=quota_used+$1,
		status=CASE WHEN quota>0 AND status='active' AND quota_used+$1>=quota THEN 'quota_exhausted' ELSE status END,
		usage_5h=CASE WHEN window_5h_start IS NULL OR window_5h_start+INTERVAL '5 hours'<=NOW() THEN $1 ELSE usage_5h+$1 END,
		usage_1d=CASE WHEN window_1d_start IS NULL OR window_1d_start+INTERVAL '24 hours'<=NOW() THEN $1 ELSE usage_1d+$1 END,
		usage_7d=CASE WHEN window_7d_start IS NULL OR window_7d_start+INTERVAL '7 days'<=NOW() THEN $1 ELSE usage_7d+$1 END,
		window_5h_start=CASE WHEN window_5h_start IS NULL OR window_5h_start+INTERVAL '5 hours'<=NOW() THEN NOW() ELSE window_5h_start END,
		window_1d_start=CASE WHEN window_1d_start IS NULL OR window_1d_start+INTERVAL '24 hours'<=NOW() THEN date_trunc('day',NOW()) ELSE window_1d_start END,
		window_7d_start=CASE WHEN window_7d_start IS NULL OR window_7d_start+INTERVAL '7 days'<=NOW() THEN date_trunc('day',NOW()) ELSE window_7d_start END,
		updated_at=NOW() WHERE id=$2 RETURNING quota>0 AND quota_used>=quota`, amount, keyID).Scan(&exhausted)
	return exhausted, err
}

func (r *usageBillingRepository) FinalizeUpstreamRejection(ctx context.Context, cmd *service.UpstreamRejectionFinalizeCommand) (*service.UpstreamRejectionBillingResult, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, balance, frozen, err := r.lockedRejection(ctx, tx, cmd.RequestID, cmd.APIKeyID)
	if err != nil {
		return nil, err
	}
	result := &service.UpstreamRejectionBillingResult{Reservation: &row.UpstreamRejectionReservation, NewBalance: balance, FrozenBalance: frozen}
	if cmd.DeadlinePassed {
		if row.State == service.UpstreamRejectionReleased && row.finalFingerprint == cmd.Fingerprint() {
			return result, nil
		}
		// Only a synchronous hold that was sent and whose durable deadline has
		// passed by the database clock. A task settles its own hold, and a NULL
		// deadline (rows created before the waiver existed) is never released.
		if (row.State != service.UpstreamRejectionSent && row.State != service.UpstreamRejectionUnknown) || row.Task != nil {
			return nil, service.ErrUpstreamRejectionState
		}
		var due bool
		if err := tx.QueryRowContext(ctx, `SELECT release_after IS NOT NULL AND release_after<=NOW() FROM upstream_rejection_reservations WHERE request_id=$1`, row.RequestID).Scan(&due); err != nil {
			return nil, err
		}
		if !due {
			return nil, service.ErrUpstreamRejectionState
		}
	} else if row.ReleasedAtDeadline() {
		// The site already absorbed this request. A rejection that arrives late
		// is kept for supplier reconciliation and never captures money; other
		// observations carry no new information.
		if cmd.Outcome != service.UpstreamRejectionRejected {
			return nil, service.ErrUpstreamRejectionWaived
		}
		if err := recordUpstreamRejectionLateResult(ctx, tx, row.RequestID, &service.UpstreamRejectionLateResult{
			Kind: service.UpstreamRejectionRejected, Amount: row.Price, Evidence: cmd.Evidence,
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, service.ErrUpstreamRejectionWaived
	}
	if row.State == cmd.Outcome {
		// A later status GET can have a different upstream request ID/body while
		// the money outcome is still unknown. Retain the first durable evidence
		// and hold; only terminal financial decisions require identical replay.
		if row.State == service.UpstreamRejectionUnknown {
			return result, nil
		}
		if row.finalFingerprint != cmd.Fingerprint() {
			return nil, service.ErrUpstreamRejectionConflict
		}
		return result, nil
	}
	if row.State != service.UpstreamRejectionReserved && row.State != service.UpstreamRejectionSent && row.State != service.UpstreamRejectionUnknown {
		return nil, service.ErrUpstreamRejectionState
	}
	if cmd.Outcome == service.UpstreamRejectionRejected && (row.State == service.UpstreamRejectionReserved || cmd.Evidence.RuleVersion != row.RuleVersion) {
		return nil, service.ErrUpstreamRejectionState
	}
	if cmd.Outcome == service.UpstreamRejectionReleased && row.State != service.UpstreamRejectionReserved && !cmd.ConfirmedNoCharge && !cmd.DeadlinePassed {
		return nil, service.ErrUpstreamRejectionState
	}
	amount := 0.0
	switch cmd.Outcome {
	case service.UpstreamRejectionRejected:
		fingerprint := service.HashUsageRequestPayload([]byte(row.fingerprint + ":" + cmd.Fingerprint()))
		applied, claimErr := r.claimUsageBillingRequest(ctx, tx, row.RequestID, row.APIKeyID, fingerprint)
		if claimErr != nil {
			return nil, claimErr
		}
		if !applied {
			return nil, service.ErrUpstreamRejectionIntegrity
		}
		err = tx.QueryRowContext(ctx, `UPDATE users SET frozen_balance=frozen_balance-$1,updated_at=NOW() WHERE id=$2 AND frozen_balance>=$1 RETURNING balance,frozen_balance`, row.Price, row.UserID).Scan(&result.NewBalance, &result.FrozenBalance)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrUpstreamRejectionIntegrity
		}
		if err != nil {
			return nil, err
		}
		result.APIKeyQuotaExhausted, err = captureRejectionKey(ctx, tx, row.APIKeyID, row.Price)
		if err != nil {
			return nil, err
		}
		// The provider's fixed cost is already final. Never multiply it by the
		// account or group multiplier. Preserve account quota outbox semantics.
		if row.UpstreamCost > 0 {
			result.AccountQuotaState, err = incrementUsageBillingAccountQuota(ctx, tx, row.AccountID, row.UpstreamCost, true, true)
			if err != nil && !errors.Is(err, service.ErrAccountNotFound) {
				return nil, err
			}
		}
		amount = row.Price
	case service.UpstreamRejectionReleased:
		result.NewBalance, result.FrozenBalance, err = releaseRejectionFunds(ctx, tx, row)
		if err != nil {
			return nil, err
		}
	case service.UpstreamRejectionUnknown:
		// No funds or key budget are released. A later verified terminal outcome
		// can resolve this state using the same server-owned operation ID.
	}
	stored := cmd.Evidence
	if cmd.DeadlinePassed {
		// Decided here from the locked row, never by the caller: a release from
		// sent still owes a later ordinary bill, one from unknown does not.
		stored.ReleasedFrom = row.State
	}
	evidence, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	// Becoming unknown without a task moves the deadline to at most one grace
	// period away. A NULL deadline gets one only when the send itself was
	// uncertain (reserved -> unknown). A sent row with a NULL deadline was
	// already waiting before deadlines existed and keeps NULL forever: it is
	// reconciled by hand. Task-bound rows keep NULL: their task settles them.
	// (LEAST ignores NULL, so the NULL case must be spelled out.)
	shorten := cmd.Outcome == service.UpstreamRejectionUnknown && row.Task == nil
	err = tx.QueryRowContext(ctx, `UPDATE upstream_rejection_reservations SET state=$2,evidence=$3::jsonb,finalization_fingerprint=$4,updated_at=NOW(),
		release_after=CASE
			WHEN NOT $5::boolean THEN release_after
			WHEN release_after IS NOT NULL THEN LEAST(release_after,NOW()+make_interval(secs=>$6::double precision))
			WHEN state='reserved' THEN NOW()+make_interval(secs=>$6::double precision)
			ELSE NULL END
		WHERE request_id=$1 RETURNING updated_at,release_after`,
		row.RequestID, cmd.Outcome, string(evidence), cmd.Fingerprint(), shorten, service.UpstreamRejectionReleaseGrace.Seconds()).Scan(&row.UpdatedAt, &row.ReleaseAfter)
	if err != nil {
		return nil, err
	}
	row.State = cmd.Outcome
	row.Evidence = stored
	if err := rejectionEvent(ctx, tx, row, cmd.Outcome, amount); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result.Applied = true
	return result, nil
}

func releaseRejectionFunds(ctx context.Context, tx *sql.Tx, row *rejectionRow) (float64, float64, error) {
	var balance, frozen float64
	err := tx.QueryRowContext(ctx, `UPDATE users SET balance=balance+$1,frozen_balance=frozen_balance-$1,updated_at=NOW() WHERE id=$2 AND frozen_balance>=$1 RETURNING balance,frozen_balance`, row.Price, row.UserID).Scan(&balance, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, service.ErrUpstreamRejectionIntegrity
	}
	return balance, frozen, err
}

// Only the new hold-aware path is stricter; preserve ordinary legacy semantics.
func validateUpstreamRejectionNormalCommand(cmd *service.UsageBillingCommand) error {
	if cmd == nil || !service.IsUpstreamRejectionRequestID(cmd.UpstreamRejectionReservationID) || cmd.SubscriptionCost != 0 {
		return service.ErrUpstreamRejectionInvalid
	}
	for _, amount := range []float64{cmd.BalanceCost, cmd.APIKeyQuotaCost, cmd.APIKeyRateLimitCost, cmd.AccountQuotaCost} {
		if amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return service.ErrUpstreamRejectionInvalid
		}
	}
	return nil
}

// Called inside ordinary Apply's transaction before claiming the ordinary bill.
// A later normal-billing error rolls back the release and its audit event too.
func (r *usageBillingRepository) releaseUpstreamRejectionForNormal(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand) error {
	if err := validateUpstreamRejectionNormalCommand(cmd); err != nil {
		return err
	}
	row, _, _, err := r.lockedRejection(ctx, tx, cmd.UpstreamRejectionReservationID, cmd.APIKeyID)
	if err != nil {
		return err
	}
	if row.UserID != cmd.UserID || row.AccountID != cmd.AccountID || row.RequestPayloadHash != cmd.RequestPayloadHash {
		return service.ErrUpstreamRejectionConflict
	}
	if row.State == service.UpstreamRejectionNormal {
		if row.NormalBillingRequestID != cmd.RequestID {
			return service.ErrUpstreamRejectionConflict
		}
		return nil
	}
	if row.ReleasedAtDeadline() {
		// From sent: the request outlived the presumed process death and really
		// completed. Only the fixed-fee hold was given back; the ordinary bill is
		// owed and the caller charges it once through the ordinary path (its
		// dedup claim makes replays free). From unknown: the site absorbed this
		// request, so the result is recorded and not charged; the caller commits
		// this record without claiming the bill.
		charged := row.ReleasedAtDeadlineWhileSent()
		if err := recordUpstreamRejectionLateResult(ctx, tx, row.RequestID, &service.UpstreamRejectionLateResult{
			Kind: service.UpstreamRejectionNormal, BillingRequestID: cmd.RequestID, Amount: cmd.BalanceCost, Charged: charged,
			Evidence: service.UpstreamRejectionBillingEvidence{
				UpstreamRequestID: cmd.UpstreamRequestID, InputTokens: cmd.InputTokens, OutputTokens: cmd.OutputTokens,
				CacheReadTokens: cmd.CacheReadTokens, CacheWriteTokens: cmd.CacheCreationTokens, OriginalModel: cmd.Model,
			},
		}); err != nil || charged {
			return err
		}
		return service.ErrUpstreamRejectionWaived
	}
	if row.State != service.UpstreamRejectionSent && row.State != service.UpstreamRejectionUnknown {
		return service.ErrUpstreamRejectionState
	}
	if _, _, err := releaseRejectionFunds(ctx, tx, row); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `UPDATE upstream_rejection_reservations SET state='normal',normal_billing_request_id=$2,updated_at=NOW() WHERE request_id=$1 RETURNING updated_at`, row.RequestID, cmd.RequestID).Scan(&row.UpdatedAt)
	if err != nil {
		return err
	}
	row.State = service.UpstreamRejectionNormal
	row.NormalBillingRequestID = cmd.RequestID
	return rejectionEvent(ctx, tx, row, row.State, cmd.BalanceCost)
}

// recordUpstreamRejectionLateResult keeps the first late result only, so
// replays never rewrite it.
func recordUpstreamRejectionLateResult(ctx context.Context, tx *sql.Tx, requestID string, late *service.UpstreamRejectionLateResult) error {
	encoded, err := json.Marshal(late)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE upstream_rejection_reservations SET late_result=$2::jsonb||jsonb_build_object('recorded_at',NOW())
		WHERE request_id=$1 AND state='released' AND late_result IS NULL`, requestID, string(encoded))
	return err
}

func (r *usageBillingRepository) BindUpstreamRejectionTask(ctx context.Context, binding *service.UpstreamRejectionTaskBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if err := r.rejectionDB(); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, _, _, err := r.lockedRejection(ctx, tx, binding.RequestID, binding.APIKeyID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if row.Task != nil {
		existing, _ := json.Marshal(row.Task)
		if string(existing) != string(encoded) {
			return service.ErrUpstreamRejectionConflict
		}
		return nil
	}
	if row.State != service.UpstreamRejectionSent && row.State != service.UpstreamRejectionUnknown {
		return service.ErrUpstreamRejectionState
	}
	// The task's terminal state settles this hold; the timer must never release it.
	err = tx.QueryRowContext(ctx, `UPDATE upstream_rejection_reservations SET task_id=$2,task_metadata=$3::jsonb,updated_at=NOW(),release_after=NULL WHERE request_id=$1 RETURNING updated_at`, binding.RequestID, binding.TaskID, string(encoded)).Scan(&row.UpdatedAt)
	if err != nil {
		return err
	}
	row.Task = binding
	if err := rejectionEvent(ctx, tx, row, "task_bound", 0); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *usageBillingRepository) GetUpstreamRejectionByTask(ctx context.Context, userID, keyID int64, taskID string) (*service.UpstreamRejectionReservation, error) {
	if userID <= 0 || keyID <= 0 || strings.TrimSpace(taskID) == "" || len(taskID) > 256 {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, rejectionSelect+` WHERE user_id=$1 AND api_key_id=$2 AND task_id=$3 LIMIT 2`, userID, keyID, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result *service.UpstreamRejectionReservation
	for rows.Next() {
		row, err := scanRejection(rows)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return nil, service.ErrUpstreamRejectionConflict
		}
		result = &row.UpstreamRejectionReservation
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, service.ErrUpstreamRejectionNotFound
	}
	return result, nil
}
