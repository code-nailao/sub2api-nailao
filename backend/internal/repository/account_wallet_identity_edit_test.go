package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func walletEditFixture() *service.Account {
	return &service.Account{ID: 42, CreatedAt: time.Unix(100, 0).UTC(), Platform: "openai", Type: "apikey", Credentials: map[string]any{
		"api_key": "test-key", "base_url": "https://wallet.example/v1", "model_mapping": map[string]any{"old": "old"},
	}}
}

func TestWalletMetadataEditOnlyIgnoresReviewedFields(t *testing.T) {
	before := walletEditFixture()
	for _, key := range []string{"model_mapping", "compact_model_mapping", "openai_capabilities"} {
		t.Run(key, func(t *testing.T) {
			after := *before
			after.Credentials = copyJSONMap(before.Credentials)
			after.Credentials[key] = "changed"
			require.NotEqual(t, service.AccountWalletIdentity(before), service.AccountWalletIdentity(&after))
			require.Equal(t, walletEditCallingIdentity(before), walletEditCallingIdentity(&after))
		})
	}
	for _, key := range []string{"api_key", "base_url", "api_base_urls", "api_protocol", "account_mode", "header_overrides", "header_override_enabled", "unknown_future_identity", "pool_mode", "pool_mode_retry_count", "temp_unschedulable_rules"} {
		t.Run(key, func(t *testing.T) {
			after := *before
			after.Credentials = copyJSONMap(before.Credentials)
			after.Credentials[key] = "changed"
			require.NotEqual(t, walletEditCallingIdentity(before), walletEditCallingIdentity(&after))
		})
	}
	for name, change := range map[string]func(*service.Account){
		"account ID":          func(a *service.Account) { a.ID++ },
		"creation generation": func(a *service.Account) { a.CreatedAt = a.CreatedAt.Add(time.Second) },
		"platform":            func(a *service.Account) { a.Platform = "anthropic" },
		"type":                func(a *service.Account) { a.Type = "oauth" },
		"parent":              func(a *service.Account) { parent := int64(8); a.ParentAccountID = &parent },
		"proxy ID":            func(a *service.Account) { proxy := int64(9); a.ProxyID = &proxy },
		"proxy URL": func(a *service.Account) {
			a.Proxy = &service.Proxy{Protocol: "http", Host: "proxy.example", Port: 8080}
		},
	} {
		t.Run(name, func(t *testing.T) {
			after := *before
			change(&after)
			require.NotEqual(t, walletEditCallingIdentity(before), walletEditCallingIdentity(&after))
		})
	}
	after := *before
	after.Extra = map[string]any{service.UpstreamModelMetadataExtraKey: map[string]any{"synced_at": "now"}}
	require.Equal(t, service.AccountWalletIdentity(before), service.AccountWalletIdentity(&after), "only refreshing capability metadata already leaves wallet identity unchanged")
	after.Extra[service.AccountWalletConfigExtraKey] = map[string]any{"url": "https://other.example"}
	require.NotEqual(t, walletEditCallingIdentity(before), walletEditCallingIdentity(&after))
}

func TestWalletMetadataEditCASRetainsOldReaderFormat(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			before := walletEditFixture()
			after := *before
			after.Credentials = copyJSONMap(before.Credentials)
			after.Credentials["model_mapping"] = map[string]any{"old": "old", "new": "new"}
			oldID, newID := service.AccountWalletIdentity(before), service.AccountWalletIdentity(&after)
			mock.ExpectBegin()
			tx, err := client.Tx(context.Background())
			require.NoError(t, err)
			mock.ExpectExec(`(?s)UPDATE sources_wallet_management.*jsonb_set.*WHERE id=1.*identity.*\$4`).
				WithArgs(`["members","42","identity"]`, newID, "42", oldID).WillReturnResult(sqlmock.NewResult(0, 1))
			query := mock.ExpectExec(regexp.QuoteMeta("UPDATE sources_account_wallet_snapshots")+`(?s).*WHERE account_id=\$2 AND identity=\$3`).WithArgs(newID, int64(42), oldID)
			if fail {
				query.WillReturnError(errors.New("snapshot write failed"))
			} else {
				query.WillReturnResult(sqlmock.NewResult(0, 1))
			}
			err = preserveWalletAfterMetadataEdit(context.Background(), tx.Client(), before, &after)
			if fail {
				require.Error(t, err)
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
			} else {
				require.NoError(t, err)
				require.Len(t, newID, 64)
				require.False(t, strings.Contains(newID, "test-key"))
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWalletMetadataEditNeverMigratesCredentialOrUnknownChanges(t *testing.T) {
	before := walletEditFixture()
	for _, key := range []string{"api_key", "base_url", "header_overrides", "future_field"} {
		t.Run(key, func(t *testing.T) {
			after := *before
			after.Credentials = copyJSONMap(before.Credentials)
			after.Credentials["model_mapping"] = map[string]any{"new": "new"}
			after.Credentials[key] = "changed"
			// nil client：若错误地执行任何数据库续接会立即失败。
			require.NoError(t, preserveWalletAfterMetadataEdit(context.Background(), nil, before, &after))
		})
	}
}
