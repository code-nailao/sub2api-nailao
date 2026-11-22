package admin

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type manualRefreshFixtureRepo struct {
	service.AdminService
	service.AccountRepository
	account     service.Account
	cache       *manualRefreshFixtureCache
	unprotected bool
	conditional bool
}

func (r *manualRefreshFixtureRepo) GetAccount(ctx context.Context, id int64) (*service.Account, error) {
	return r.GetByID(ctx, id)
}

func (r *manualRefreshFixtureRepo) GetByID(context.Context, int64) (*service.Account, error) {
	copy := r.account
	copy.Credentials = maps.Clone(copy.Credentials)
	return &copy, nil
}

func (r *manualRefreshFixtureRepo) UpdateAccount(ctx context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	r.unprotected = true
	r.account.Credentials = input.Credentials
	return r.GetByID(ctx, id)
}

func (r *manualRefreshFixtureRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, _ int64, expected map[string]any, _ *int64, credentials map[string]any) (bool, error) {
	r.conditional = r.cache.held
	if expected["refresh_token"] != r.account.Credentials["refresh_token"] {
		return false, nil
	}
	r.account.Credentials = credentials
	return true, nil
}

func (*manualRefreshFixtureRepo) EnsureOpenAIPrivacy(context.Context, *service.Account) string {
	return ""
}
func (*manualRefreshFixtureRepo) EnsureAntigravityPrivacy(context.Context, *service.Account) string {
	return ""
}

type manualRefreshFixtureCache struct {
	service.GeminiTokenCache
	held        bool
	invalidated bool
}

func (c *manualRefreshFixtureCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	if c.held {
		return false, nil
	}
	c.held = true
	return true, nil
}

func (c *manualRefreshFixtureCache) ReleaseRefreshLock(context.Context, string) error {
	c.held = false
	return nil
}

func (c *manualRefreshFixtureCache) DeleteAccessToken(context.Context, string) error {
	c.invalidated = true
	return nil
}

type manualRefreshFixtureClient struct {
	service.OpenAIOAuthClient
	beforeReturn func()
}

func (c manualRefreshFixtureClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	c.beforeReturn()
	return &openai.TokenResponse{AccessToken: "fixture-attempt-access", RefreshToken: "fixture-attempt-refresh"}, nil
}

func TestManualOpenAIRefreshPreservesConcurrentReauthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dedicatedEndpoint := range []bool{false, true} {
		name := "shared_account_refresh"
		if dedicatedEndpoint {
			name = "openai_oauth_endpoint"
		}
		t.Run(name, func(t *testing.T) {
			cache := &manualRefreshFixtureCache{}
			repo := &manualRefreshFixtureRepo{cache: cache, account: service.Account{
				ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusError,
				Credentials: map[string]any{"access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh"},
			}}
			client := manualRefreshFixtureClient{beforeReturn: func() {
				// Reauthorization commits after the exchange and before its write.
				repo.account.Credentials = map[string]any{"access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh"}
			}}
			api := service.NewOAuthRefreshAPI(repo, cache)
			oauth := service.ProvideOpenAIOAuthService(nil, client, nil, api)
			t.Cleanup(oauth.Stop)
			if dedicatedEndpoint {
				h := NewOpenAIOAuthHandler(oauth, repo, nil, nil)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/accounts/1/refresh", nil)
				c.Params = gin.Params{{Key: "id", Value: "1"}}
				h.RefreshAccountToken(c)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			} else {
				h := &AccountHandler{adminService: repo, openaiOAuthService: oauth}
				account, err := repo.GetAccount(t.Context(), repo.account.ID)
				require.NoError(t, err)
				updated, _, err := h.refreshSingleAccount(t.Context(), account)
				require.NoError(t, err)
				require.Equal(t, "fixture-new-refresh", updated.GetOpenAIRefreshToken())
			}
			require.Equal(t, "fixture-new-refresh", repo.account.GetOpenAIRefreshToken())
			require.True(t, repo.conditional, "credential compare-and-set runs while the refresh lock is held")
			require.False(t, repo.unprotected)
			require.False(t, cache.held, "the refresh lock is released afterwards")
		})
	}
}

func TestManualOpenAIRefreshHoldsTheSharedLockUntilTransactionEnds(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			entClient := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := entClient.Tx(t.Context())
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			ctx := dbent.NewTxContext(t.Context(), tx)

			cache := &manualRefreshFixtureCache{}
			repo := &manualRefreshFixtureRepo{cache: cache, account: service.Account{
				ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive,
				Credentials: map[string]any{"access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh"},
			}}
			calls := 0
			client := manualRefreshFixtureClient{beforeReturn: func() { calls++ }}
			oauth := service.ProvideOpenAIOAuthService(nil, client, nil, service.NewOAuthRefreshAPI(repo, cache))
			t.Cleanup(oauth.Stop)
			h := &AccountHandler{adminService: repo, openaiOAuthService: oauth}
			account, err := repo.GetAccount(ctx, repo.account.ID)
			require.NoError(t, err)
			_, _, err = h.refreshSingleAccount(ctx, account)
			require.NoError(t, err)
			require.True(t, cache.held)
			require.False(t, cache.invalidated)

			_, _, err = h.refreshSingleAccount(t.Context(), account)
			require.Equal(t, http.StatusConflict, infraerrors.Code(err))
			require.Equal(t, 1, calls)
			if commit {
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
			} else {
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
			}
			require.Equal(t, commit, cache.invalidated)
			require.False(t, cache.held)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
