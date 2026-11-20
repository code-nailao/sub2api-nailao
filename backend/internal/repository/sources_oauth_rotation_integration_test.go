//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
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

// Cancel the original HTTP request once PostgreSQL has accepted the CAS, but
// before ScopedAdminWrite can commit the transaction.
type cancelAfterOAuthCAS struct {
	*accountRepository
	cancel context.CancelFunc
}

func (r cancelAfterOAuthCAS) UpdateOpenAIOAuthCredentialsIfUnchanged(ctx context.Context, id int64, expected map[string]any, proxyID *int64, credentials map[string]any) (bool, error) {
	applied, err := r.accountRepository.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, id, expected, proxyID, credentials)
	if applied && err == nil {
		r.cancel()
	}
	return applied, err
}

type scopedOAuthRotationClient struct {
	service.OpenAIOAuthClient
	checkExchange func(context.Context)
}

func (c scopedOAuthRotationClient) RefreshTokenWithClientID(ctx context.Context, _, _, _ string) (*openai.TokenResponse, error) {
	c.checkExchange(ctx)
	return &openai.TokenResponse{AccessToken: "fixture-next-access", RefreshToken: "fixture-next-refresh", ExpiresIn: 3600}, nil
}

func TestScopedOAuthRotationPersistsAfterHTTPContextCancellation(t *testing.T) {
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	account := &service.Account{
		Name: "scoped-canceled-rotation-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Concurrency: 1,
		Credentials: map[string]any{"access_token": "fixture-prior-access", "refresh_token": "fixture-prior-refresh"},
	}
	require.NoError(t, repo.Create(t.Context(), account))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, repo.Delete(ctx, account.ID))
	})
	principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
		Permissions: []string{"accounts.refresh"}, AccountTypes: []string{service.AccountTypeOAuth},
		Platforms: []string{service.PlatformOpenAI}, IncludeUngrouped: true,
	}}}}
	requestCtx, cancelRequest := context.WithCancel(adminaccess.WithPrincipal(t.Context(), principal))
	defer cancelRequest()
	admin := service.NewAdminService(nil, nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		integrationEntClient, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	scoped := admin.(service.ScopedAdminAccounts)
	canceled, cancelBeforeRefresh := context.WithCancel(adminaccess.WithPrincipal(t.Context(), principal))
	cancelBeforeRefresh()
	_, err := scoped.ScopedAdminWrite(canceled, account.ID, "accounts.refresh", nil, func(context.Context, *service.Account) (*service.Account, error) {
		t.Fatal("a request cancelled before the exchange must not call the OAuth provider")
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)

	refreshAPI := service.NewOAuthRefreshAPI(cancelAfterOAuthCAS{accountRepository: repo, cancel: cancelRequest}, nil)
	oauth := service.ProvideOpenAIOAuthService(nil, scopedOAuthRotationClient{checkExchange: func(ctx context.Context) {
		require.Nil(t, dbent.TxFromContext(ctx), "the OAuth provider must run outside the credentials write transaction")
	}}, nil, refreshAPI)
	t.Cleanup(oauth.Stop)

	updated, resErr := scoped.ScopedAdminWrite(requestCtx, account.ID, "accounts.refresh", nil, oauth.RefreshStoredAccount)

	require.ErrorIs(t, requestCtx.Err(), context.Canceled, "cancel only after the real conditional update succeeds")
	durable, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "fixture-next-refresh", durable.GetOpenAIRefreshToken(), "cancelling HTTP must not roll back rotated credentials")
	require.Equal(t, "fixture-next-access", durable.GetOpenAIAccessToken())
	require.NoError(t, resErr)
	require.Equal(t, durable.GetOpenAIRefreshToken(), updated.GetOpenAIRefreshToken())
	var locks int
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `
		SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&locks))
	require.Zero(t, locks, "no advisory lock may be left on a pooled session")
}
