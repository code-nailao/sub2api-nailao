//go:build integration

package repository

import (
	"testing"

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
