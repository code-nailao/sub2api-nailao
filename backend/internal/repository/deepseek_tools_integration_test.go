//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type deepSeekIntegrationFixture struct {
	user     *service.User
	group    *service.Group
	accounts []*service.Account
}

func newDeepSeekIntegrationFixture(t *testing.T) *deepSeekIntegrationFixture {
	t.Helper()
	f := &deepSeekIntegrationFixture{}
	// Settlement owns real transactions, so these fixtures cannot use the
	// harness rollback transaction. Register cleanup before creating any rows.
	t.Cleanup(func() { require.NoError(t, f.cleanup()) })
	return f
}

func (f *deepSeekIntegrationFixture) cleanup() error {
	var userID, groupID int64
	if f.user != nil {
		userID = f.user.ID
	}
	if f.group != nil {
		groupID = f.group.ID
	}
	var accountIDs pq.Int64Array
	for _, account := range f.accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var keyIDs pq.Int64Array
	var keyHashes pq.StringArray
	if err := tx.QueryRowContext(ctx, `SELECT ARRAY(SELECT id FROM api_keys WHERE user_id=$1),
		ARRAY(SELECT encode(sha256(convert_to(key,'UTF8')),'hex') FROM api_keys WHERE user_id=$1)`, userID).Scan(&keyIDs, &keyHashes); err != nil {
		return err
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM deepseek_usage_journal WHERE user_id=$1 OR api_key_id=ANY($2) OR account_id=ANY($3)`, []any{userID, keyIDs, accountIDs}},
		{`DELETE FROM usage_logs WHERE user_id=$1 OR api_key_id=ANY($2) OR account_id=ANY($3)`, []any{userID, keyIDs, accountIDs}},
		{`DELETE FROM usage_billing_dedup WHERE api_key_id=ANY($1)`, []any{keyIDs}},
		{`DELETE FROM usage_billing_dedup_archive WHERE api_key_id=ANY($1)`, []any{keyIDs}},
		{`DELETE FROM usage_group_daily_rollups WHERE group_id=$1`, []any{groupID}},
		{`DELETE FROM api_keys WHERE user_id=$1`, []any{userID}},
		{`DELETE FROM users WHERE id=$1`, []any{userID}},
		{`DELETE FROM accounts WHERE id=ANY($1)`, []any{accountIDs}},
		{`DELETE FROM groups WHERE id=$1`, []any{groupID}},
		// Key/account deletion can enqueue invalidations; remove those last.
		{`DELETE FROM scheduler_outbox WHERE account_id=ANY($1) OR group_id=$2`, []any{accountIDs, groupID}},
		{`DELETE FROM auth_cache_invalidation_outbox WHERE cache_key=ANY($1)`, []any{keyHashes}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("clean DeepSeek fixture: %w", err)
		}
	}
	return tx.Commit()
}

func TestDeepSeekConfigurationPersistenceAndAuthProjection(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	fixture := newDeepSeekIntegrationFixture(t)
	price := 6.0
	accountID := int64(7181)
	group := &service.Group{Name: "deepseek-" + uuid.NewString(), Status: service.StatusActive, Platform: service.PlatformDeepseek, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: .35, SearchPricePer1k: &price, DeepSeekTools: service.DeepSeekToolsConfig{Enabled: true, AutoInject: true, AccountID: &accountID, FetchPricePer1K: &price}}
	fixture.group = group
	require.NoError(t, NewGroupRepository(client, integrationDB).Create(ctx, group))
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@fixture.test", Balance: 1})
	fixture.user = user
	repo := NewAPIKeyRepository(client, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-fixture-" + uuid.NewString(), Name: "DeepSeek", Status: service.StatusActive, DeepSeekToolMode: "disabled"}
	require.NoError(t, repo.Create(ctx, key))
	got, err := repo.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, "disabled", got.DeepSeekToolMode)
	require.Equal(t, group.DeepSeekTools, got.Group.DeepSeekTools)
	require.False(t, got.DeepSeekToolsAutoInject())
	key.DeepSeekToolMode = "enabled"
	require.NoError(t, repo.Update(ctx, key, service.APIKeyUpdateFields{DeepSeekToolMode: true}))
	got, err = repo.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.True(t, got.DeepSeekToolsAutoInject())
	group.DeepSeekTools.AutoInject = false
	group.DeepSeekTools.FetchPricePer1K = new(float64)
	require.NoError(t, NewGroupRepository(client, integrationDB).Update(ctx, group))
	got, err = repo.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.Zero(t, *got.Group.DeepSeekTools.FetchPricePer1K, "explicit free price must survive persistence")
}

func deepSeekJournalFixture(t *testing.T) (*usageBillingRepository, *service.DeepSeekUsageCheckpoint, int64) {
	t.Helper()
	ctx := context.Background()
	client := testEntClient(t)
	fixture := newDeepSeekIntegrationFixture(t)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@fixture.test", Balance: 1})
	fixture.user = user
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-fixture-" + uuid.NewString(), Name: "deepseek", Quota: 1})
	account := mustCreateAccount(t, client, &service.Account{Name: "deepseek-model-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{"quota_limit": 1.0}})
	fixture.accounts = append(fixture.accounts, account)
	supplier := mustCreateAccount(t, client, &service.Account{Name: "deepseek-tool-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{"quota_limit": 1.0}})
	fixture.accounts = append(fixture.accounts, supplier)
	requestID := "deepseek-tools:" + uuid.NewString()
	usage := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: requestID, Model: "deepseek-flash", RequestedModel: "deepseek-flash", TotalCost: .052, ActualCost: .026, RateMultiplier: .35, CreatedAt: time.Now(), BillingSnapshot: &service.UsageBillingSnapshot{Version: 1, ChargeKind: "deepseek_tools", DeepSeekTools: &service.DeepSeekToolUsage{SearchCount: 1, FetchCount: 1, SearchPrice: .006, FetchPrice: .006, SupplierAccountID: supplier.ID, SupplierSearchPrice: .005, SupplierFetchPrice: .005}}}
	cmd := &service.UsageBillingCommand{RequestID: requestID, APIKeyID: key.ID, UserID: user.ID, AccountID: account.ID, AccountType: service.AccountTypeAPIKey, Model: "deepseek-flash", BalanceCost: .026, APIKeyQuotaCost: .026, APIKeyRateLimitCost: .026, AccountQuotaCost: .04, ToolAccountID: supplier.ID, ToolAccountQuotaCost: .01, DeepSeekToolFingerprint: "1:1:0.006:0.006:fixture:0.005:0.005"}
	repo := &usageBillingRepository{db: integrationDB}
	require.NoError(t, repo.BeginDeepSeekUsage(ctx, requestID, key.ID, user.ID, account.ID, time.Now().Add(time.Minute)))
	return repo, &service.DeepSeekUsageCheckpoint{Command: cmd, Usage: usage, Revision: 1}, supplier.ID
}

func TestDeepSeekJournalAtomicSettlementAndConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	repo, checkpoint, supplierID := deepSeekJournalFixture(t)
	require.NoError(t, repo.SaveDeepSeekUsage(ctx, checkpoint))
	waiting, err := repo.SettleDeepSeekUsage(ctx, checkpoint.Command.RequestID, checkpoint.Command.APIKeyID)
	require.NoError(t, err)
	require.Nil(t, waiting, "live checkpoints cannot be settled by recovery")
	checkpoint.Ready = true
	checkpoint.Revision++
	require.NoError(t, repo.SaveDeepSeekUsage(ctx, checkpoint))
	var applied atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := repo.SettleDeepSeekUsage(ctx, checkpoint.Command.RequestID, checkpoint.Command.APIKeyID)
			if err != nil {
				errors <- err
				return
			}
			if out.Result.Applied {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), applied.Load())
	var balance, quota, accountCost, supplierCost float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1`, checkpoint.Command.UserID).Scan(&balance))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT quota_used FROM api_keys WHERE id=$1`, checkpoint.Command.APIKeyID).Scan(&quota))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric FROM accounts WHERE id=$1`, checkpoint.Command.AccountID).Scan(&accountCost))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric FROM accounts WHERE id=$1`, supplierID).Scan(&supplierCost))
	require.InDelta(t, .974, balance, 1e-10)
	require.InDelta(t, .026, quota, 1e-10)
	require.InDelta(t, .04, accountCost, 1e-10)
	require.InDelta(t, .01, supplierCost, 1e-10)
	var rows int
	var actual float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*),SUM(actual_cost) FROM usage_logs WHERE request_id=$1`, checkpoint.Command.RequestID).Scan(&rows, &actual))
	require.Equal(t, 1, rows)
	require.InDelta(t, .026, actual, 1e-10)
	checkpoint.Command.RequestFingerprint = "different"
	checkpoint.Revision++
	require.ErrorIs(t, repo.SaveDeepSeekUsage(ctx, checkpoint), service.ErrUsageBillingRequestConflict)
}

func TestDeepSeekJournalCrashRecoveryUsesOnlySavedEvidence(t *testing.T) {
	ctx := context.Background()
	repo, checkpoint, _ := deepSeekJournalFixture(t)
	require.NoError(t, repo.SaveDeepSeekUsage(ctx, checkpoint))
	_, err := integrationDB.ExecContext(ctx, `UPDATE deepseek_usage_journal SET deadline=NOW()-INTERVAL '1 minute' WHERE request_id=$1`, checkpoint.Command.RequestID)
	require.NoError(t, err)
	// A new repository represents a new process. It has no provider or model
	// client and settles only the last confirmed monetary snapshot.
	restarted := &usageBillingRepository{db: integrationDB}
	rows, err := restarted.ListPendingDeepSeekUsage(ctx, 100)
	require.NoError(t, err)
	require.Contains(t, rows, service.DeepSeekUsageIdentity{RequestID: checkpoint.Command.RequestID, APIKeyID: checkpoint.Command.APIKeyID})
	first, err := restarted.SettleDeepSeekUsage(ctx, checkpoint.Command.RequestID, checkpoint.Command.APIKeyID)
	require.NoError(t, err)
	require.True(t, first.Result.Applied)
	second, err := restarted.SettleDeepSeekUsage(ctx, checkpoint.Command.RequestID, checkpoint.Command.APIKeyID)
	require.NoError(t, err)
	require.False(t, second.Result.Applied)
	require.Equal(t, first.Command.RequestFingerprint, second.Command.RequestFingerprint)

	emptyID := "deepseek-tools:" + uuid.NewString()
	require.NoError(t, restarted.BeginDeepSeekUsage(ctx, emptyID, checkpoint.Command.APIKeyID, checkpoint.Command.UserID, checkpoint.Command.AccountID, time.Now().Add(-time.Minute)))
	empty, err := restarted.SettleDeepSeekUsage(ctx, emptyID, checkpoint.Command.APIKeyID)
	require.NoError(t, err)
	require.Nil(t, empty.Command, "unconfirmed operation cannot be inferred or charged")
	var n int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id=$1`, emptyID).Scan(&n))
	require.Zero(t, n)
}

func TestDeepSeekJournalUsageInsertFailureRollsBackCharge(t *testing.T) {
	ctx := context.Background()
	repo, checkpoint, _ := deepSeekJournalFixture(t)
	checkpoint.Ready = true
	checkpoint.Usage.Model = fmt.Sprintf("%0300d", 1)
	require.NoError(t, repo.SaveDeepSeekUsage(ctx, checkpoint))
	_, err := repo.SettleDeepSeekUsage(ctx, checkpoint.Command.RequestID, checkpoint.Command.APIKeyID)
	require.Error(t, err)
	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1`, checkpoint.Command.UserID).Scan(&balance))
	require.Equal(t, 1.0, balance)
	var state string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM deepseek_usage_journal WHERE request_id=$1`, checkpoint.Command.RequestID).Scan(&state))
	require.Equal(t, "ready", state)
}
