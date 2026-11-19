//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSourcesImageLedgerAtomicTerminalOperations(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)
	ledger := repo.(service.SourcesImageLedger)
	user := mustCreateUser(t, client, &service.User{Email: "media-engine-" + uuid.NewString() + "@example.test", PasswordHash: "hash", Balance: 100})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "test-" + uuid.NewString(), Name: "Media engine fixture", Quota: 100})
	account := mustCreateAccount(t, client, &service.Account{Name: "Media engine fixture " + uuid.NewString(), Type: service.AccountTypeAPIKey})
	makeHold := func(amount float64) *service.BatchImageBalanceHoldCommand {
		id := "mediatask_" + uuid.NewString()
		cmd := &service.BatchImageBalanceHoldCommand{RequestID: service.BatchImageHoldRequestID(id), BatchID: id, UserID: user.ID, APIKeyID: key.ID, HoldAmount: amount, RequestPayloadHash: "fixture"}
		_, err := repo.ReserveBatchImageBalance(ctx, cmd)
		require.NoError(t, err)
		return cmd
	}
	capture := func(h *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldCommand, *service.UsageBillingCommand) {
		c := *h
		c.RequestID = service.BatchImageCaptureRequestID(h.BatchID)
		c.ActualAmount = c.HoldAmount
		c.RequestFingerprint = ""
		b := &service.UsageBillingCommand{RequestID: "media-engine-image:" + h.BatchID, UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, AccountType: service.AccountTypeAPIKey, APIKeyQuotaCost: h.HoldAmount, APIKeyRateLimitCost: h.HoldAmount, RequestPayloadHash: h.RequestPayloadHash}
		return &c, b
	}
	release := func(h *service.BatchImageBalanceHoldCommand) *service.BatchImageBalanceHoldCommand {
		c := *h
		c.RequestID = service.BatchImageReleaseRequestID(h.BatchID)
		c.RequestFingerprint = ""
		return &c
	}
	check := func(balance, frozen, used float64) {
		var b, f, q float64
		require.NoError(t, integrationDB.QueryRow("SELECT balance,frozen_balance FROM users WHERE id=$1", user.ID).Scan(&b, &f))
		require.NoError(t, integrationDB.QueryRow("SELECT quota_used FROM api_keys WHERE id=$1", key.ID).Scan(&q))
		require.InDelta(t, balance, b, 1e-8)
		require.InDelta(t, frozen, f, 1e-8)
		require.InDelta(t, used, q, 1e-8)
	}
	a, b := makeHold(3), makeHold(7)
	check(90, 10, 0)
	c, bill := capture(a)
	res, err := ledger.SettleSourcesImage(ctx, c, bill)
	require.NoError(t, err)
	require.True(t, res.Applied)
	check(90, 7, 3)
	// A stale SQLite restore must not release another task's frozen funds.
	require.ErrorIs(t, ledger.ReleaseSourcesImage(ctx, release(a)), service.ErrUsageBillingRequestConflict)
	check(90, 7, 3)
	res, err = ledger.SettleSourcesImage(ctx, c, bill)
	require.NoError(t, err)
	require.False(t, res.Applied)
	check(90, 7, 3)
	require.NoError(t, ledger.ReleaseSourcesImage(ctx, release(b)))
	require.NoError(t, ledger.ReleaseSourcesImage(ctx, release(b)))
	check(97, 0, 3)
	c, bill = capture(b)
	_, err = ledger.SettleSourcesImage(ctx, c, bill)
	require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
	// Capture and release race through the same original receipt row lock.
	d := makeHold(5)
	c, bill = capture(d)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, err := ledger.SettleSourcesImage(ctx, c, bill); results <- err }()
	go func() { defer wg.Done(); results <- ledger.ReleaseSourcesImage(ctx, release(d)) }()
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if err == service.ErrUsageBillingRequestConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
	var balance, frozen, quota float64
	require.NoError(t, integrationDB.QueryRow("SELECT balance,frozen_balance FROM users WHERE id=$1", user.ID).Scan(&balance, &frozen))
	require.NoError(t, integrationDB.QueryRow("SELECT quota_used FROM api_keys WHERE id=$1", key.ID).Scan(&quota))
	require.Equal(t, 0.0, frozen)
	require.InDelta(t, 100, balance+quota, 1e-8)
	// Inject failure after capture, during native quota effects. Full rollback must
	// restore both money and receipts; retry after repository reconstruction wins once.
	e := makeHold(4)
	c, bill = capture(e)
	fn := fmt.Sprintf("sources_fail_quota_%d", key.ID)
	trigger := fn + "_trigger"
	_, err = integrationDB.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.id=%d THEN RAISE EXCEPTION 'synthetic quota outage'; END IF; RETURN NEW; END$$; CREATE TRIGGER %s BEFORE UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION %s()`, fn, key.ID, trigger, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON api_keys; DROP FUNCTION IF EXISTS %s()", trigger, fn))
	})
	_, err = ledger.SettleSourcesImage(ctx, c, bill)
	require.Error(t, err)
	var n int
	require.NoError(t, integrationDB.QueryRow("SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1", c.RequestID).Scan(&n))
	require.Zero(t, n)
	check(balance-4, 4, quota)
	_, err = integrationDB.Exec(fmt.Sprintf("DROP TRIGGER %s ON api_keys; DROP FUNCTION %s()", trigger, fn))
	require.NoError(t, err)
	rebuilt := NewUsageBillingRepository(client, integrationDB).(service.SourcesImageLedger)
	_, err = rebuilt.SettleSourcesImage(ctx, c, bill)
	require.NoError(t, err)
	check(balance-4, 0, quota+4)
}

// Ledger batch ids carry the task id prefix of the deployment. The legacy
// values are deliberately fake.
func TestSourcesImageLedgerBatchIDFollowsNamespace(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)
	ledger := repo.(service.SourcesImageLedger)
	user := mustCreateUser(t, client, &service.User{Email: "media-engine-ns-" + uuid.NewString() + "@example.test", PasswordHash: "hash", Balance: 100})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "test-" + uuid.NewString(), Name: "Media engine namespace fixture", Quota: 100})
	account := mustCreateAccount(t, client, &service.Account{Name: "Media engine namespace fixture " + uuid.NewString(), Type: service.AccountTypeAPIKey})
	t.Cleanup(func() { service.SetSourcesMediaEngineLegacy("", "") })
	settle := func(prefix, billingPrefix string) error {
		id := prefix + uuid.NewString()
		hold := &service.BatchImageBalanceHoldCommand{RequestID: service.BatchImageHoldRequestID(id), BatchID: id, UserID: user.ID, APIKeyID: key.ID, HoldAmount: 1, RequestPayloadHash: "fixture"}
		if _, err := repo.ReserveBatchImageBalance(ctx, hold); err != nil {
			return err
		}
		c := *hold
		c.RequestID, c.ActualAmount, c.RequestFingerprint = service.BatchImageCaptureRequestID(id), c.HoldAmount, ""
		bill := &service.UsageBillingCommand{RequestID: billingPrefix + id, UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, AccountType: service.AccountTypeAPIKey, APIKeyQuotaCost: 1, APIKeyRateLimitCost: 1, RequestPayloadHash: "fixture"}
		_, err := ledger.SettleSourcesImage(ctx, &c, bill)
		return err
	}

	service.SetSourcesMediaEngineLegacy("legacyname", "legacytask")
	require.NoError(t, settle("legacytask_", "legacyname-image:"))
	require.NoError(t, settle("mediatask_", "media-engine-image:"), "the neutral form is accepted too")
	require.ErrorIs(t, settle("othertask_", "legacyname-image:"), service.ErrUsageBillingRequestConflict)

	service.SetSourcesMediaEngineLegacy("", "")
	require.NoError(t, settle("mediatask_", "media-engine-image:"))
	require.ErrorIs(t, settle("legacytask_", "media-engine-image:"), service.ErrUsageBillingRequestConflict, "a legacy batch id needs the legacy prefix configured")
}
