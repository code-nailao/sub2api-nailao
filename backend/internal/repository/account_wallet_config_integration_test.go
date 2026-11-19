//go:build integration

package repository

import (
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWalletConfigCASPreservesModelCredentialsAndConcurrentExtra(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(t.Context(), tx)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := &service.Account{Name: "wallet-config-integration", Platform: "openai", Type: "apikey", Status: "active", Concurrency: 1, Priority: 50,
		Credentials: map[string]any{"api_key": "isolated-key", "base_url": "https://direct.example/v1"},
		Extra:       map[string]any{"quota_used": 7, service.AccountWalletConfigExtraKey: map[string]any{"url": "https://injected.example"}}}
	require.NoError(t, repo.Create(ctx, a))
	loaded, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.NotContains(t, loaded.Extra, service.AccountWalletConfigExtraKey, "create/import cannot bypass testing")
	beforeCredentials := loaded.Credentials
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{"quota_used": 9, "concurrent": "preserved"}))
	config := map[string]any{"url": "https://wallet.example", "version": "first"}
	require.NoError(t, repo.UpdateAccountWalletConfig(ctx, loaded, config))
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, beforeCredentials, current.Credentials)
	require.Equal(t, config, current.Extra[service.AccountWalletConfigExtraKey])
	require.EqualValues(t, 9, current.Extra["quota_used"])
	require.Equal(t, "preserved", current.Extra["concurrent"])
	require.ErrorIs(t, repo.UpdateAccountWalletConfig(ctx, loaded, map[string]any{"url": "https://other.example", "version": "stale"}), service.ErrWalletConfigChanged)
	// Ordinary edits loaded before the wallet save must keep the latest subtree.
	loaded.Name = "renamed"
	loaded.Extra = map[string]any{"ordinary": "edited", service.AccountWalletConfigExtraKey: map[string]any{"url": "https://injected.example"}}
	require.NoError(t, repo.Update(ctx, loaded))
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, config, current.Extra[service.AccountWalletConfigExtraKey])
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountWalletConfigExtraKey: nil, "unrelated": true}))
	_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Extra: map[string]any{service.AccountWalletConfigExtraKey: map[string]any{"url": "https://injected.example"}, "bulk": true}})
	require.NoError(t, err)
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, config, current.Extra[service.AccountWalletConfigExtraKey])
	require.Equal(t, true, current.Extra["unrelated"])
	require.Equal(t, true, current.Extra["bulk"])
	// Reset retains a version, so an older test cannot be replayed after A -> B -> A.
	reset := map[string]any{"url": "", "version": "reset"}
	require.NoError(t, repo.UpdateAccountWalletConfig(ctx, current, reset))
	require.ErrorIs(t, repo.UpdateAccountWalletConfig(ctx, current, config), service.ErrWalletConfigChanged)
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	changedKey := *current
	changedKey.Credentials = map[string]any{"api_key": "rotated", "base_url": "https://direct.example/v1"}
	require.NoError(t, repo.Update(ctx, &changedKey))
	require.ErrorIs(t, repo.UpdateAccountWalletConfig(ctx, current, config), service.ErrWalletConfigChanged)
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	current.CreatedAt = current.CreatedAt.Add(-time.Second)
	require.ErrorIs(t, repo.UpdateAccountWalletConfig(ctx, current, config), service.ErrWalletConfigChanged)
}
