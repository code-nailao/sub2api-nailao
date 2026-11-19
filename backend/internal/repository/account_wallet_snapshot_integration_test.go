//go:build integration

package repository

import (
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWalletSnapshotsPersistLeaseScheduleAndIdentity(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(t.Context(), tx)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := &service.Account{Name: "wallet-snapshot-fixture", Platform: "openai", Type: "apikey", Status: "active", Concurrency: 1, Priority: 50,
		Credentials: map[string]any{"api_key": "isolated-secret", "base_url": "https://wallet.example"}}
	require.NoError(t, accounts.Create(ctx, a))
	// Production readers use the persisted timestamp precision for the identity.
	a, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	repo := &accountWalletSnapshotRepository{sql: tx}
	other := &accountWalletSnapshotRepository{sql: tx}
	identity := service.AccountWalletIdentity(a)
	claimed, err := repo.Claim(ctx, a.ID, identity, "scheduled-first", true)
	require.NoError(t, err)
	require.False(t, claimed)
	row, err := repo.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, row)
	claimed, err = repo.Claim(ctx, a.ID, identity, "first", false)
	require.NoError(t, err)
	require.True(t, claimed)
	var leaseSeconds float64
	leaseRows, err := tx.QueryContext(ctx, `SELECT EXTRACT(EPOCH FROM (lease_until - NOW())) FROM sources_account_wallet_snapshots WHERE account_id = $1`, a.ID)
	require.NoError(t, err)
	require.True(t, leaseRows.Next())
	require.NoError(t, leaseRows.Scan(&leaseSeconds))
	require.NoError(t, leaseRows.Close())
	require.GreaterOrEqual(t, leaseSeconds, service.AccountWalletOperationTimeout.Seconds())

	claimed, err = other.Claim(ctx, a.ID, identity, "duplicate", false)
	require.NoError(t, err)
	require.False(t, claimed)
	claimed, err = other.Claim(ctx, a.ID, "changed", "other-identity", false)
	require.NoError(t, err)
	require.False(t, claimed, "identity edit cannot steal a live query")
	now, next := time.Now().UTC(), time.Now().UTC().Add(5*time.Minute)
	value := service.AccountWallet{Status: "ready", Remaining: "0.000000001", Unit: "USD", CheckedAt: &now, LastAttemptAt: &now, NextCheckAt: &next, AutoRefresh: true}
	saved, err := repo.Complete(ctx, a.ID, identity, "wrong-token", value)
	require.NoError(t, err)
	require.False(t, saved)
	saved, err = repo.Complete(ctx, a.ID, identity, "first", value)
	require.NoError(t, err)
	require.True(t, saved)
	row, err = other.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, value, row.Value)
	due, err := repo.Due(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, due)
	claimed, err = repo.Claim(ctx, a.ID, identity, "too-soon", false)
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = tx.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET next_check_at = NOW() - INTERVAL '1 second', last_started_at = NOW() - INTERVAL '1 minute' WHERE account_id = $1`, a.ID)
	require.NoError(t, err)
	due, err = repo.Due(ctx, 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	claimed, err = repo.Claim(ctx, a.ID, identity, "background", true)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, other.Release(ctx, a.ID, "wrong-token"))
	due, err = repo.Due(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, due)
	// An abandoned lease expires; a late writer cannot overwrite its successor.
	_, err = tx.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET lease_until = NOW() - INTERVAL '1 second', last_started_at = NOW() - INTERVAL '1 minute' WHERE account_id = $1`, a.ID)
	require.NoError(t, err)
	claimed, err = other.Claim(ctx, a.ID, identity, "recovered", true)
	require.NoError(t, err)
	require.True(t, claimed)
	saved, err = repo.Complete(ctx, a.ID, identity, "background", value)
	require.NoError(t, err)
	require.False(t, saved)
	require.NoError(t, other.Release(ctx, a.ID, "recovered"))
	// A stale identity cannot reset a snapshot, even after the lease is released.
	claimed, err = other.Claim(ctx, a.ID, "changed", "stale-reader", false)
	require.NoError(t, err)
	require.False(t, claimed)
	// A genuinely new credential must start with an empty snapshot and no schedule.
	a.Credentials["api_key"] = "replacement-key"
	require.NoError(t, accounts.Update(ctx, a))
	changedIdentity := service.AccountWalletIdentity(a)
	claimed, err = other.Claim(ctx, a.ID, changedIdentity, "replacement", false)
	require.NoError(t, err)
	require.True(t, claimed)
	row, err = repo.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Empty(t, row.Value.Remaining)
	require.False(t, row.Value.AutoRefresh)
	require.NoError(t, repo.Discard(ctx, a.ID, identity))
	row, err = repo.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, row, "old identity cannot delete a new snapshot")
	require.NoError(t, repo.Discard(ctx, a.ID, changedIdentity))
	row, err = repo.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, row)
	current, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.Credentials, current.Credentials)
}
