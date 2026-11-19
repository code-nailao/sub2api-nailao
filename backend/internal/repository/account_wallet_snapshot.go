package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountWalletSnapshotRepository struct{ sql sqlExecutor }

func NewAccountWalletSnapshotRepository(db *sql.DB) service.AccountWalletSnapshotRepository {
	return &accountWalletSnapshotRepository{sql: db}
}

func (r *accountWalletSnapshotRepository) Get(ctx context.Context, id int64) (*service.AccountWalletSnapshot, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT identity, value, coalesce(lease_until > NOW(), false) FROM sources_account_wallet_snapshots WHERE account_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	snapshot := &service.AccountWalletSnapshot{AccountID: id}
	var raw []byte
	if err := rows.Scan(&snapshot.Identity, &raw, &snapshot.InFlight); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &snapshot.Value); err != nil {
		return nil, err
	}
	return snapshot, rows.Err()
}

// Claim serializes with account edits before taking the snapshot lease. A reader
// holding a pre-edit account must not reset a newly migrated snapshot to its old
// identity. Existing transaction callers keep the same lock until their commit.
func (r *accountWalletSnapshotRepository) Claim(ctx context.Context, id int64, identity, token string, scheduled bool) (bool, error) {
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(claimCtx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
		claimed, err := (&accountWalletSnapshotRepository{sql: tx}).claimLocked(claimCtx, id, identity, token, scheduled)
		if err != nil {
			return false, err
		}
		return claimed, tx.Commit()
	}
	return r.claimLocked(claimCtx, id, identity, token, scheduled)
}

func (r *accountWalletSnapshotRepository) claimLocked(ctx context.Context, id int64, identity, token string, scheduled bool) (bool, error) {
	matched, err := r.lockAndMatchIdentity(ctx, id, identity)
	if err != nil || !matched {
		return false, err
	}
	// The schedule may only refresh an already successful, identical wallet.
	// Manual queries can enroll/reset identity. Neither path steals a live lease.
	var result sql.Result
	if !scheduled {
		result, err = r.sql.ExecContext(ctx, `
		INSERT INTO sources_account_wallet_snapshots AS old (account_id, identity, lease_token, lease_until)
		SELECT id, $2, $3, NOW() + ($4 * INTERVAL '1 second') FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		ON CONFLICT (account_id) DO UPDATE SET
			identity = EXCLUDED.identity,
			value = CASE WHEN old.identity = EXCLUDED.identity THEN old.value ELSE '{"status":"pending","stale":false}'::jsonb END,
			next_check_at = CASE WHEN old.identity = EXCLUDED.identity THEN old.next_check_at ELSE NULL END,
			lease_token = EXCLUDED.lease_token, lease_until = EXCLUDED.lease_until,
			last_started_at = NOW(), updated_at = NOW()
		WHERE (old.lease_until IS NULL OR old.lease_until <= NOW())
			AND (old.identity <> EXCLUDED.identity OR old.last_started_at <= NOW() - INTERVAL '15 seconds')
	`, id, identity, token, int(service.AccountWalletLeaseDuration.Seconds()))
	} else {
		result, err = r.sql.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots
			SET lease_token = $3, lease_until = NOW() + ($4 * INTERVAL '1 second'), last_started_at = NOW(), updated_at = NOW()
			WHERE account_id = $1 AND identity = $2 AND next_check_at <= NOW()
				AND (lease_until IS NULL OR lease_until <= NOW())
				AND last_started_at <= NOW() - INTERVAL '15 seconds'`, id, identity, token, int(service.AccountWalletLeaseDuration.Seconds()))
	}
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *accountWalletSnapshotRepository) Complete(ctx context.Context, id int64, identity, token string, value service.AccountWallet) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots
		SET value = $4::jsonb, next_check_at = $5, lease_token = NULL, lease_until = NULL, updated_at = NOW()
		WHERE account_id = $1 AND identity = $2 AND lease_token = $3`, id, identity, token, string(raw), value.NextCheckAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *accountWalletSnapshotRepository) Release(ctx context.Context, id int64, token string) error {
	_, err := r.sql.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET lease_token = NULL, lease_until = NULL WHERE account_id = $1 AND lease_token = $2`, id, token)
	return err
}

func (r *accountWalletSnapshotRepository) Due(ctx context.Context, limit int) ([]service.AccountWalletSnapshot, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT s.account_id, s.identity FROM sources_account_wallet_snapshots s
		JOIN accounts a ON a.id = s.account_id AND a.deleted_at IS NULL
		WHERE s.next_check_at <= NOW() AND (s.lease_until IS NULL OR s.lease_until <= NOW())
		ORDER BY s.next_check_at, s.account_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []service.AccountWalletSnapshot
	for rows.Next() {
		var row service.AccountWalletSnapshot
		if err := rows.Scan(&row.AccountID, &row.Identity); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *accountWalletSnapshotRepository) Discard(ctx context.Context, id int64, identity string) error {
	_, err := r.sql.ExecContext(ctx, `DELETE FROM sources_account_wallet_snapshots WHERE account_id = $1 AND identity = $2`, id, identity)
	return err
}

// Match the exact existing Go digest, including the current proxy URL, rather
// than duplicating its JSON encoding in SQL. Lock only the account: proxy edits
// use the opposite lock order and remain subject to the reader's final check.
func (r *accountWalletSnapshotRepository) lockAndMatchIdentity(ctx context.Context, id int64, identity string) (bool, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT a.id, a.created_at, a.platform, a.type,
		a.parent_account_id, a.credentials, a.proxy_id, a.extra,
		p.id, COALESCE(p.protocol,''), COALESCE(p.host,''), COALESCE(p.port,0),
		COALESCE(p.username,''), COALESCE(p.password,'')
		FROM accounts a LEFT JOIN proxies p ON p.id=a.proxy_id AND p.deleted_at IS NULL
		WHERE a.id=$1 AND a.deleted_at IS NULL FOR NO KEY UPDATE OF a`, id)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return false, rows.Err()
	}
	var a service.Account
	var proxy service.Proxy
	var proxyID *int64
	var credentials, extra []byte
	if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Platform, &a.Type, &a.ParentAccountID,
		&credentials, &a.ProxyID, &extra, &proxyID, &proxy.Protocol, &proxy.Host,
		&proxy.Port, &proxy.Username, &proxy.Password); err != nil {
		return false, err
	}
	if err := json.Unmarshal(credentials, &a.Credentials); err != nil {
		return false, err
	}
	if len(extra) > 0 {
		if err := json.Unmarshal(extra, &a.Extra); err != nil {
			return false, err
		}
	}
	if proxyID != nil {
		proxy.ID = *proxyID
		a.Proxy = &proxy
	}
	return service.AccountWalletIdentity(&a) == identity, rows.Err()
}
