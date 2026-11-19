package repository

import (
	"context"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The compare-and-swap changes only the wallet subtree. Model routing, keys,
// balances, quotas and concurrently updated extra fields are never rewritten.
func (r *accountRepository) UpdateAccountWalletConfig(ctx context.Context, account *service.Account, config map[string]any) error {
	if account == nil {
		return service.ErrAccountNilInput
	}
	if dbent.TxFromContext(ctx) != nil {
		return r.updateAccountWalletConfigInTx(ctx, account, config)
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		return r.updateAccountWalletConfigInTx(ctx, account, config)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.updateAccountWalletConfigInTx(dbent.NewTxContext(ctx, tx), account, config); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.syncSchedulerAccountSnapshot(ctx, account.ID)
	return nil
}

func (r *accountRepository) updateAccountWalletConfigInTx(ctx context.Context, account *service.Account, config map[string]any) error {
	payload, err := json.Marshal(map[string]any{service.AccountWalletConfigExtraKey: config})
	if err != nil {
		return err
	}
	credentials, err := json.Marshal(normalizeJSONMap(account.Credentials))
	if err != nil {
		return err
	}
	expected, err := json.Marshal(account.Extra[service.AccountWalletConfigExtraKey])
	if err != nil {
		return err
	}
	client := clientFromContext(ctx, r.client)
	matched, err := lockAndMatchProbeProxyIdentity(ctx, client, account)
	if err != nil {
		return err
	}
	if !matched {
		return service.ErrWalletConfigChanged
	}
	var proxyID any
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	result, err := client.ExecContext(ctx, `
		UPDATE accounts
		SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb, updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
			AND platform = $3 AND type = $4 AND credentials = $5::jsonb
			AND proxy_id IS NOT DISTINCT FROM $6
			AND COALESCE(extra -> 'sources_wallet_query', 'null'::jsonb) = $7::jsonb
			AND created_at = $8 AND parent_account_id IS NOT DISTINCT FROM $9
	`, string(payload), account.ID, account.Platform, account.Type, string(credentials), proxyID,
		string(expected), account.CreatedAt, account.ParentAccountID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return service.ErrWalletConfigChanged
	}
	return enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, nil)
}

// Imports, creation, bulk edits and runtime patches cannot bypass wallet testing.
func stripWalletConfig(extra map[string]any) map[string]any {
	if _, ok := extra[service.AccountWalletConfigExtraKey]; !ok {
		return extra
	}
	copy := copyJSONMap(extra)
	delete(copy, service.AccountWalletConfigExtraKey)
	return copy
}
