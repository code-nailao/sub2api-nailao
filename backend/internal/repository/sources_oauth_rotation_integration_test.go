//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthRotationPreservesTheNewerCredentials(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(t.Context(), tx)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	previous := map[string]any{"access_token": "fixture-initial-access", "refresh_token": "fixture-initial-refresh"}
	newer := map[string]any{"access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh"}
	stale := map[string]any{"access_token": "fixture-stale-access", "refresh_token": "fixture-stale-refresh"}
	account := &service.Account{
		Name: "rotation-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Concurrency: 1, Credentials: previous,
	}
	require.NoError(t, repo.Create(ctx, account))
	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, previous, nil, newer)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, previous, nil, stale)
	require.NoError(t, err)
	require.False(t, applied)
	durable, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, newer, durable.Credentials)
}

func TestOpenAIOAuthRotationUsesTheScopedTransaction(t *testing.T) {
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	before := map[string]any{"access_token": "fixture-original-access", "refresh_token": "fixture-original-refresh"}
	after := map[string]any{"access_token": "fixture-rotated-access", "refresh_token": "fixture-rotated-refresh"}
	account := &service.Account{
		Name: "scoped-rotation-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Concurrency: 1, Credentials: before,
	}
	require.NoError(t, repo.Create(t.Context(), account))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, repo.Delete(ctx, account.ID))
	})
	tx := testEntTx(t)
	ctx, cancel := context.WithTimeout(dbent.NewTxContext(t.Context(), tx), 2*time.Second)
	defer cancel()
	_, err := tx.Client().ExecContext(ctx, "SELECT id FROM accounts WHERE id = $1 FOR UPDATE", account.ID)
	require.NoError(t, err)

	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, before, nil, after)
	require.NoError(t, err)
	require.True(t, applied)
	uncommitted, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, after, uncommitted.Credentials)
	require.NoError(t, tx.Rollback())

	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, before, current.Credentials)
}
