//go:build integration

package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// These cases use the real PostgreSQL repository and migration, not a SQL mock.
// The process harness owns the isolated database; every fixture has unique IDs.
type rejectionBillingFixture struct {
	user    *service.User
	key     *service.APIKey
	account *service.Account
	group   *service.Group
	repo    service.UsageBillingRepository
	ledger  service.UpstreamRejectionBillingRepository
}

func newRejectionBillingFixture(t *testing.T, balance float64) rejectionBillingFixture {
	t.Helper()
	client := testEntClient(t)
	f := rejectionBillingFixture{}
	// The repository owns real transactions, so these fixtures cannot use the
	// harness rollback transaction. Always clean their exact identities, even
	// when a later assertion/fixture construction fails. Keeping usage rows here
	// would pollute the dashboard suites that run later in the same database.
	t.Cleanup(func() { require.NoError(t, cleanupRejectionBillingFixture(f)) })
	suffix := uuid.NewString()
	group := mustCreateGroup(t, client, &service.Group{
		Name: "rejection-group-" + suffix, Platform: service.PlatformGrok,
		SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 2,
	})
	f.group = group
	user := mustCreateUser(t, client, &service.User{
		Email: "rejection-" + suffix + "@example.test", Balance: balance,
	})
	f.user = user
	key := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, GroupID: &group.ID, Key: "test-rejection-" + suffix,
		Name: "rejection fixture", Quota: 100, RateLimit5h: 100, RateLimit1d: 100, RateLimit7d: 100,
	})
	f.key = key
	account := mustCreateAccount(t, client, &service.Account{
		Name: "rejection-account-" + suffix, Platform: service.PlatformGrok,
		Type: service.AccountTypeAPIKey,
	})
	f.account = account
	repo := NewUsageBillingRepository(client, integrationDB)
	ledger, ok := repo.(service.UpstreamRejectionBillingRepository)
	require.True(t, ok, "production repository must implement durable rejection billing")
	f.repo, f.ledger = repo, ledger
	return f
}

func cleanupRejectionBillingFixture(f rejectionBillingFixture) error {
	var userID, accountID, groupID int64
	if f.user != nil {
		userID = f.user.ID
	}
	if f.account != nil {
		accountID = f.account.ID
	}
	if f.group != nil {
		groupID = f.group.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Include additional keys created by a test, and retain hashes before the
	// DELETE trigger enqueues one final auth-cache invalidation for each key.
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
		// Corrupt-projection tests deliberately alter user/account attribution.
		// The key still belongs to this fixture even if the assertion failed
		// before repairing that projection, so owner-only cleanup is insufficient.
		{`DELETE FROM usage_logs WHERE user_id=$1 OR api_key_id=ANY($2) OR account_id=$3`, []any{userID, keyIDs, accountID}},
		{`DELETE FROM usage_billing_dedup WHERE api_key_id=ANY($1)`, []any{keyIDs}},
		{`DELETE FROM usage_billing_dedup_archive WHERE api_key_id=ANY($1)`, []any{keyIDs}},
		{`DELETE FROM upstream_rejection_billing_events WHERE user_id=$1 OR request_id IN
			(SELECT request_id FROM upstream_rejection_reservations WHERE user_id=$1 OR account_id=$2 OR group_id=$3)`, []any{userID, accountID, groupID}},
		{`DELETE FROM upstream_rejection_reservations WHERE user_id=$1 OR account_id=$2 OR group_id=$3`, []any{userID, accountID, groupID}},
		{`DELETE FROM usage_group_daily_rollups WHERE group_id=$1`, []any{groupID}},
		{`DELETE FROM api_keys WHERE user_id=$1`, []any{userID}},
		{`DELETE FROM users WHERE id=$1`, []any{userID}},
		{`DELETE FROM accounts WHERE id=$1`, []any{accountID}},
		{`DELETE FROM groups WHERE id=$1`, []any{groupID}},
		{`DELETE FROM scheduler_outbox WHERE account_id=$1 OR group_id=$2`, []any{accountID, groupID}},
		{`DELETE FROM auth_cache_invalidation_outbox WHERE cache_key=ANY($1)`, []any{keyHashes}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("clean rejection fixture: %w", err)
		}
	}
	return tx.Commit()
}

func (f rejectionBillingFixture) command() *service.UpstreamRejectionReserveCommand {
	return &service.UpstreamRejectionReserveCommand{
		RequestID: service.NewUpstreamRejectionRequestID(), UserID: f.user.ID,
		APIKeyID: f.key.ID, AccountID: f.account.ID, GroupID: f.group.ID,
		Model: "grok-4.7", RequestPayloadHash: strings.Repeat("a", 64),
		UpstreamIdentityHash: strings.Repeat("d", 64),
		PolicyVersion:        "fixture-policy-v1", RuleVersion: "grok_content_rejection_v1",
		ProviderProfile: "grok_content_rejection_v1", Price: 0.1,
		UpstreamCost: 0.1, UpstreamCurrency: "USD",
	}
}

func rejectedBillingCommand(c *service.UpstreamRejectionReserveCommand) *service.UpstreamRejectionFinalizeCommand {
	return &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionRejected,
		Evidence: service.UpstreamRejectionBillingEvidence{
			RuleID: "fixture-content-policy", RuleVersion: c.RuleVersion, OriginalStatus: 403,
			UpstreamRequestID: "upstream-" + uuid.NewString(), ResponseDigest: strings.Repeat("b", 64),
		},
	}
}

func reserveAndSendRejection(t *testing.T, f rejectionBillingFixture, c *service.UpstreamRejectionReserveCommand) {
	t.Helper()
	result, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.True(t, result.Applied)
}

func assertRejectionMoney(t *testing.T, f rejectionBillingFixture, balance, frozen, used string) {
	t.Helper()
	var b, h, q string
	require.NoError(t, integrationDB.QueryRow(`SELECT balance::text,frozen_balance::text FROM users WHERE id=$1`, f.user.ID).Scan(&b, &h))
	require.NoError(t, integrationDB.QueryRow(`SELECT quota_used::text FROM api_keys WHERE id=$1`, f.key.ID).Scan(&q))
	for _, pair := range [][2]string{{balance, b}, {frozen, h}, {used, q}} {
		want, err := decimal.NewFromString(pair[0])
		require.NoError(t, err)
		got, err := decimal.NewFromString(pair[1])
		require.NoError(t, err)
		require.True(t, want.Equal(got), "money mismatch: want %s, got %s", want, got)
	}
}

func assertRejectionReceiptCount(t *testing.T, c *service.UpstreamRejectionReserveCommand, kind string, count int) {
	t.Helper()
	var actual int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind=$2`, c.RequestID, kind).Scan(&actual))
	require.Equal(t, count, actual)
}

func TestUpstreamRejectionReserveDurableSnapshotAndReplay(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	// Historical frozen money belongs to other tasks and must remain untouched.
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	first, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	require.True(t, first.Applied)
	assertRejectionMoney(t, f, "9.9", "7.1", "0")
	second, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	require.False(t, second.Applied)
	assertRejectionMoney(t, f, "9.9", "7.1", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReserved, 1)
	// Reconstructing the repository simulates a different process with no cache.
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	stored, err := rebuilt.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, *c, stored.UpstreamRejectionReserveCommand)
	require.Equal(t, service.UpstreamRejectionReserved, stored.State)
	require.False(t, stored.CreatedAt.IsZero())
	_, err = rebuilt.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID+1000000)
	require.Error(t, err, "a key must not read another key's reservation")
}

func TestUpstreamRejectionReserveSnapshotConflict(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	_, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	mutations := map[string]func(*service.UpstreamRejectionReserveCommand){
		"price":    func(x *service.UpstreamRejectionReserveCommand) { x.Price = 0.2 },
		"payload":  func(x *service.UpstreamRejectionReserveCommand) { x.RequestPayloadHash = strings.Repeat("c", 64) },
		"identity": func(x *service.UpstreamRejectionReserveCommand) { x.UpstreamIdentityHash = strings.Repeat("e", 64) },
		"model":    func(x *service.UpstreamRejectionReserveCommand) { x.Model = "grok-4.6" },
		"policy":   func(x *service.UpstreamRejectionReserveCommand) { x.PolicyVersion = "fixture-policy-v2" },
		"cost":     func(x *service.UpstreamRejectionReserveCommand) { x.UpstreamCost = 0.2 },
		"account":  func(x *service.UpstreamRejectionReserveCommand) { x.AccountID++ },
		"group":    func(x *service.UpstreamRejectionReserveCommand) { x.GroupID++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := *c
			mutate(&changed)
			_, err := f.ledger.ReserveUpstreamRejection(context.Background(), &changed)
			require.Error(t, err)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
		})
	}
}

func TestUpstreamRejectionRejectsInvalidCommandsBeforeMoneyEffects(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	cases := map[string]func(*service.UpstreamRejectionReserveCommand){
		"client-id":              func(c *service.UpstreamRejectionReserveCommand) { c.RequestID = "client:reused" },
		"zero-price":             func(c *service.UpstreamRejectionReserveCommand) { c.Price = 0 },
		"negative":               func(c *service.UpstreamRejectionReserveCommand) { c.Price = -0.1 },
		"nan":                    func(c *service.UpstreamRejectionReserveCommand) { c.Price = math.NaN() },
		"infinity":               func(c *service.UpstreamRejectionReserveCommand) { c.Price = math.Inf(1) },
		"too-precise":            func(c *service.UpstreamRejectionReserveCommand) { c.Price = 0.100000001 },
		"currency":               func(c *service.UpstreamRejectionReserveCommand) { c.UpstreamCurrency = "CNY" },
		"profile":                func(c *service.UpstreamRejectionReserveCommand) { c.ProviderProfile = "wildcard" },
		"hash":                   func(c *service.UpstreamRejectionReserveCommand) { c.RequestPayloadHash = "client-input" },
		"identity-empty":         func(c *service.UpstreamRejectionReserveCommand) { c.UpstreamIdentityHash = "" },
		"identity-nonhex":        func(c *service.UpstreamRejectionReserveCommand) { c.UpstreamIdentityHash = strings.Repeat("z", 64) },
		"negative-supplier-cost": func(c *service.UpstreamRejectionReserveCommand) { c.UpstreamCost = -0.1 },
		"rule":                   func(c *service.UpstreamRejectionReserveCommand) { c.RuleVersion = "" },
		"policy":                 func(c *service.UpstreamRejectionReserveCommand) { c.PolicyVersion = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := f.command()
			mutate(c)
			_, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
			require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
			assertRejectionMoney(t, f, "10", "0", "0")
		})
	}
}

func TestUpstreamRejectionReserveConcurrentBalanceAcrossKeys(t *testing.T) {
	f := newRejectionBillingFixture(t, 0.2)
	otherKey := mustCreateApiKey(t, testEntClient(t), &service.APIKey{
		UserID: f.user.ID, GroupID: &f.group.ID, Key: "test-rejection-other-" + uuid.NewString(), Quota: 100,
	})
	const attempts = 20
	errorsCh := make(chan error, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		cmd := f.command()
		if i%2 == 0 {
			cmd.APIKeyID = otherKey.ID
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.ledger.ReserveUpstreamRejection(context.Background(), cmd)
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	accepted := 0
	for err := range errorsCh {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, service.ErrInsufficientBalance)
		}
	}
	require.Equal(t, 2, accepted, "distinct keys must share the same authoritative user balance")
	assertRejectionMoney(t, f, "0", "0.2", "0")
}

func TestUpstreamRejectionReserveConcurrentKeyBudgets(t *testing.T) {
	limits := map[string]error{
		"quota":         service.ErrAPIKeyQuotaExhausted,
		"rate_limit_5h": service.ErrAPIKeyRateLimit5hExceeded,
		"rate_limit_1d": service.ErrAPIKeyRateLimit1dExceeded,
		"rate_limit_7d": service.ErrAPIKeyRateLimit7dExceeded,
	}
	for column, exhaustedErr := range limits {
		t.Run(column, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			_, err := integrationDB.Exec(`UPDATE api_keys SET `+pq.QuoteIdentifier(column)+`=0.1 WHERE id=$1`, f.key.ID)
			require.NoError(t, err)
			const attempts = 12
			results := make(chan error, attempts)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < attempts; i++ {
				cmd := f.command()
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := f.ledger.ReserveUpstreamRejection(context.Background(), cmd)
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				} else {
					require.ErrorIs(t, err, exhaustedErr, "an unexpected DB error is not a budget rejection")
				}
			}
			require.Equal(t, 1, accepted, "pending fees count against %s before capture", column)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
		})
	}
}

func TestUpstreamRejectionWindowResetAndFixedCapture(t *testing.T) {
	for _, window := range []string{"NULL", "NOW()-INTERVAL '8 days'"} {
		t.Run(window, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			_, err := integrationDB.Exec(`UPDATE api_keys SET usage_5h=17,usage_1d=17,usage_7d=17,
				rate_limit_5h=0.1,rate_limit_1d=0.1,rate_limit_7d=0.1,
				window_5h_start=`+window+`,window_1d_start=`+window+`,window_7d_start=`+window+` WHERE id=$1`, f.key.ID)
			require.NoError(t, err)
			c := f.command()
			reserveAndSendRejection(t, f, c)
			result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
			require.NoError(t, err)
			require.True(t, result.Applied)
			assertRejectionMoney(t, f, "9.9", "0", "0.1")
			var h, d, w float64
			require.NoError(t, integrationDB.QueryRow(`SELECT usage_5h,usage_1d,usage_7d FROM api_keys WHERE id=$1`, f.key.ID).Scan(&h, &d, &w))
			require.Equal(t, 0.1, h)
			require.Equal(t, 0.1, d)
			require.Equal(t, 0.1, w)
		})
	}
}

func TestUpstreamRejectionCaptureConcurrentOnceAndImmutablePrice(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := integrationDB.Exec(`UPDATE groups SET rate_multiplier=9 WHERE id=$1`, f.group.ID)
	require.NoError(t, err)
	finalize := rejectedBillingCommand(c)
	const attempts = 16
	results := make(chan *service.UpstreamRejectionBillingResult, attempts)
	errorsCh := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
			results <- res
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}
	applied := 0
	for res := range results {
		require.NotNil(t, res)
		if res.Applied {
			applied++
		}
	}
	require.Equal(t, 1, applied)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, c.RequestID, c.APIKeyID).Scan(&count))
	require.Equal(t, 1, count)
	changed := *finalize
	changed.Evidence.OriginalStatus = 400
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &changed)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionConflict)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
}

func TestUpstreamRejectionUnknownRetainsFundsUntilConfirmed(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE api_keys SET quota=0.1 WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	unknown := &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown}
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), unknown)
	require.NoError(t, err)
	// A fresh repository sees the durable pending state even with no Redis client.
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	stored, err := rebuilt.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionUnknown, stored.State)
	_, err = rebuilt.ReserveUpstreamRejection(context.Background(), f.command())
	require.Error(t, err, "unknown still reserves the key budget")
	release := &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionReleased}
	_, err = rebuilt.FinalizeUpstreamRejection(context.Background(), release)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	release.ConfirmedNoCharge = true
	_, err = rebuilt.FinalizeUpstreamRejection(context.Background(), release)
	require.NoError(t, err)
	_, err = rebuilt.FinalizeUpstreamRejection(context.Background(), release)
	require.NoError(t, err)
	assertRejectionMoney(t, f, "10", "0", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
	_, err = rebuilt.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	require.Error(t, err, "a released reservation must not capture other frozen money")
}

func TestUpstreamRejectionSendAndStateGuards(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	_, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "unsent requests cannot be charged")
	_, err = f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID+1000000)
	require.Error(t, err)
	first, err := f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.True(t, first.Applied)
	second, err := f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.False(t, second.Applied, "the caller must not send again after an ambiguous retry")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionSent, 1)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
}

func TestUpstreamRejectionHoldCannotConsumeAnotherReservation(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	a, b := f.command(), f.command()
	reserveAndSendRejection(t, f, a)
	_, err = f.ledger.ReserveUpstreamRejection(context.Background(), b)
	require.NoError(t, err)
	finalize := rejectedBillingCommand(a)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
		RequestID: a.RequestID, APIKeyID: a.APIKeyID, Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true,
	})
	require.Error(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(f.command()))
	require.Error(t, err, "an arbitrary request ID cannot withdraw from frozen_balance")
	assertRejectionMoney(t, f, "9.8", "7.1", "0.1")
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
		RequestID: b.RequestID, APIKeyID: b.APIKeyID, Outcome: service.UpstreamRejectionReleased,
	})
	require.NoError(t, err)
	assertRejectionMoney(t, f, "9.9", "7", "0.1")
}

// failRejectionEvent installs a narrowly scoped, real PostgreSQL failure. The
// event insert occurs in the money transaction, so all prior mutations roll back.
func failRejectionEvent(t *testing.T, requestID, kind string) func() {
	t.Helper()
	fn := "rejection_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	trigger := fn + "_trigger"
	_, err := integrationDB.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.request_id=%s AND NEW.kind=%s THEN RAISE EXCEPTION 'synthetic rejection ledger failure'; END IF; RETURN NEW; END$$;
		CREATE TRIGGER %s BEFORE INSERT ON upstream_rejection_billing_events FOR EACH ROW EXECUTE FUNCTION %s()`,
		pq.QuoteIdentifier(fn), pq.QuoteLiteral(requestID), pq.QuoteLiteral(kind), pq.QuoteIdentifier(trigger), pq.QuoteIdentifier(fn)))
	require.NoError(t, err)
	cleanup := func() {
		_, err := integrationDB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON upstream_rejection_billing_events; DROP FUNCTION IF EXISTS %s()`, pq.QuoteIdentifier(trigger), pq.QuoteIdentifier(fn)))
		require.NoError(t, err)
	}
	t.Cleanup(cleanup)
	return cleanup
}

func TestUpstreamRejectionReserveFailureRollsBackMoneyAndReceipt(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	removeFailure := failRejectionEvent(t, c.RequestID, service.UpstreamRejectionReserved)
	_, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.Error(t, err)
	assertRejectionMoney(t, f, "10", "0", "0")
	_, err = f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionNotFound)
	removeFailure()
	_, err = f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
}

func TestUpstreamRejectionCaptureFailureRollsBackAndRecoversOnce(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	finalize := rejectedBillingCommand(c)
	removeFailure := failRejectionEvent(t, c.RequestID, service.UpstreamRejectionRejected)
	_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
	var dedup int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, c.RequestID).Scan(&dedup))
	require.Zero(t, dedup)
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionSent, stored.State)
	removeFailure()
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	for i := 0; i < 2; i++ {
		res, err := rebuilt.FinalizeUpstreamRejection(context.Background(), finalize)
		require.NoError(t, err)
		require.Equal(t, i == 0, res.Applied)
	}
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
}

func TestUpstreamRejectionNormalSettlementAtomicAndMutuallyExclusive(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	bill := &service.UsageBillingCommand{
		RequestID: "normal-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
		RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
	}
	removeFailure := failRejectionEvent(t, c.RequestID, service.UpstreamRejectionNormal)
	_, err := f.repo.Apply(context.Background(), bill)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	var dedup int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&dedup))
	require.Zero(t, dedup)
	removeFailure()
	for i := 0; i < 2; i++ {
		result, err := f.repo.Apply(context.Background(), bill)
		require.NoError(t, err)
		require.Equal(t, i == 0, result.Applied)
	}
	assertRejectionMoney(t, f, "9.975", "0", "0.025")
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionNormal, stored.State)
	require.Equal(t, bill.RequestID, stored.NormalBillingRequestID)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	require.Error(t, err, "normal and fixed bills must be mutually exclusive")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 1)
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
}

func TestUpstreamRejectionNormalSettlementRejectsForeignHold(t *testing.T) {
	f, other := newRejectionBillingFixture(t, 10), newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := other.repo.Apply(context.Background(), &service.UsageBillingCommand{
		RequestID: "normal-" + uuid.NewString(), UserID: other.user.ID, APIKeyID: other.key.ID,
		AccountID: other.account.ID, AccountType: service.AccountTypeAPIKey,
		BalanceCost: 0.025, APIKeyQuotaCost: 0.025, UpstreamRejectionReservationID: c.RequestID,
	})
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionMoney(t, other, "10", "0", "0")
}

// V1 reserves fixed rejection fees; it deliberately does not change the legacy
// ordinary billing path into a global hard-cap reservation system. An ordinary
// in-flight bill may consume quota after this hold was admitted. Keep both real
// charges in the ledger instead of silently dropping a previously promised fee.
func TestUpstreamRejectionOrdinaryBillingBoundary(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE api_keys SET quota=0.2 WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	ordinary := &service.UsageBillingCommand{
		RequestID: "ordinary-boundary-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		BalanceCost: 0.15, APIKeyQuotaCost: 0.15, APIKeyRateLimitCost: 0.15,
	}
	require.Empty(t, ordinary.UpstreamRejectionReservationID, "exercise the unchanged ordinary path")
	result, err := f.repo.Apply(context.Background(), ordinary)
	require.NoError(t, err)
	require.True(t, result.Applied)
	assertRejectionMoney(t, f, "9.75", "0.1", "0.15")
	_, err = f.ledger.ReserveUpstreamRejection(context.Background(), f.command())
	require.ErrorIs(t, err, service.ErrAPIKeyQuotaExhausted,
		"new admission includes settled ordinary usage and the outstanding fixed-fee promise")
	assertRejectionMoney(t, f, "9.75", "0.1", "0.15")
	finalize := rejectedBillingCommand(c)
	for i := 0; i < 2; i++ {
		captured, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
		require.NoError(t, err)
		require.Equal(t, i == 0, captured.Applied)
	}
	assertRejectionMoney(t, f, "9.75", "0", "0.25")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, c.RequestID, c.APIKeyID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestUpstreamRejectionNormalAndFixedRaceHaveOneTerminalWinner(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	ordinary := &service.UsageBillingCommand{
		RequestID: "normal-race-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
		RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
	}
	finalize := rejectedBillingCommand(c)
	type outcome struct {
		applied bool
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		result, err := f.repo.Apply(context.Background(), ordinary)
		results <- outcome{applied: result != nil && result.Applied, err: err}
	}()
	go func() {
		defer wg.Done()
		<-start
		result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
		results <- outcome{applied: result != nil && result.Applied, err: err}
	}()
	close(start)
	wg.Wait()
	close(results)
	applied, conflicts := 0, 0
	for result := range results {
		if result.err == nil {
			require.True(t, result.applied)
			applied++
		} else {
			require.ErrorIs(t, result.err, service.ErrUpstreamRejectionState)
			conflicts++
		}
	}
	require.Equal(t, 1, applied)
	require.Equal(t, 1, conflicts)
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	switch stored.State {
	case service.UpstreamRejectionNormal:
		assertRejectionMoney(t, f, "9.975", "0", "0.025")
	case service.UpstreamRejectionRejected:
		assertRejectionMoney(t, f, "9.9", "0", "0.1")
	default:
		t.Fatalf("unexpected terminal state: %s", stored.State)
	}
	var events, claims int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind IN ('normal','rejected')`, c.RequestID).Scan(&events))
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1 AND request_id IN ($2,$3)`, c.APIKeyID, c.RequestID, ordinary.RequestID).Scan(&claims))
	require.Equal(t, 1, events)
	require.Equal(t, 1, claims)
}

func TestUpstreamRejectionNormalLaterQuotaFailureRollsBackReleaseAndDeduction(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	bill := &service.UsageBillingCommand{
		RequestID: "normal-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
		RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
	}
	fn := "rejection_quota_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	trigger := fn + "_trigger"
	_, err := integrationDB.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.id=%d THEN RAISE EXCEPTION 'synthetic normal quota failure'; END IF; RETURN NEW; END$$;
		CREATE TRIGGER %s BEFORE UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION %s()`,
		pq.QuoteIdentifier(fn), f.key.ID, pq.QuoteIdentifier(trigger), pq.QuoteIdentifier(fn)))
	require.NoError(t, err)
	cleanup := func() {
		_, err := integrationDB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON api_keys; DROP FUNCTION IF EXISTS %s()`, pq.QuoteIdentifier(trigger), pq.QuoteIdentifier(fn)))
		require.NoError(t, err)
	}
	t.Cleanup(cleanup)
	_, err = f.repo.Apply(context.Background(), bill)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 0)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&count))
	require.Zero(t, count, "native dedup claim must roll back with the later quota failure")
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionSent, stored.State)
	cleanup()
	result, err := f.repo.Apply(context.Background(), bill)
	require.NoError(t, err)
	require.True(t, result.Applied)
	assertRejectionMoney(t, f, "9.975", "0", "0.025")
}

func TestUpstreamRejectionCaptureRequiresValidEvidence(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	for _, status := range []int{0, 302, 401, 404, 429, 500} {
		t.Run(fmt.Sprintf("http-%d", status), func(t *testing.T) {
			cmd := rejectedBillingCommand(c)
			cmd.Evidence.OriginalStatus = status
			_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), cmd)
			require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
		})
	}
	cmd := rejectedBillingCommand(c)
	cmd.Evidence.RuleVersion = "unrelated-provider-rules"
	_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), cmd)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
}

func TestUpstreamRejectionEightDecimalFeeIsConservedExactly(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	c.Price = 0.00000001
	reserveAndSendRejection(t, f, c)
	_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	require.NoError(t, err)
	assertRejectionMoney(t, f, "9.99999999", "0", "0.00000001")
	var amount string
	require.NoError(t, integrationDB.QueryRow(`SELECT amount::text FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind='rejected'`, c.RequestID).Scan(&amount))
	require.Equal(t, "0.00000001", amount)
}

func TestUpstreamRejectionTaskBindingIsDurableAndOwnerScoped(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	c.Model = "grok-imagine-video-1.5"
	reserveAndSendRejection(t, f, c)
	binding := &service.UpstreamRejectionTaskBinding{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, TaskID: "video-" + uuid.NewString(),
		DurationSeconds: 8, Resolution: "480p", OriginalModel: c.Model, UpstreamModel: c.Model,
	}
	require.NoError(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding))
	require.NoError(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding))
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	stored, err := rebuilt.GetUpstreamRejectionByTask(context.Background(), f.user.ID, f.key.ID, binding.TaskID)
	require.NoError(t, err)
	require.Equal(t, c.RequestID, stored.RequestID)
	require.Equal(t, binding, stored.Task)
	_, err = rebuilt.GetUpstreamRejectionByTask(context.Background(), f.user.ID+1000000, f.key.ID, binding.TaskID)
	require.Error(t, err)
	_, err = rebuilt.GetUpstreamRejectionByTask(context.Background(), f.user.ID, f.key.ID+1000000, binding.TaskID)
	require.Error(t, err)
	changed := *binding
	changed.DurationSeconds = 16
	require.Error(t, rebuilt.BindUpstreamRejectionTask(context.Background(), &changed))
	second := f.command()
	second.Model = c.Model
	reserveAndSendRejection(t, f, second)
	changed = *binding
	changed.RequestID = second.RequestID
	require.Error(t, rebuilt.BindUpstreamRejectionTask(context.Background(), &changed), "same provider task cannot bind twice")
	assertRejectionReceiptCount(t, c, "task_bound", 1)
	assertRejectionMoney(t, f, "9.8", "0.2", "0")
}

func TestUpstreamRejectionCancelledAdmissionDoesNotFreezeMoney(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.ledger.ReserveUpstreamRejection(ctx, f.command())
	require.Error(t, err)
	assertRejectionMoney(t, f, "10", "0", "0")
}

func TestUpstreamRejectionAdmissionRejectsInactiveIdentity(t *testing.T) {
	for _, cause := range []string{"user-disabled", "user-deleted", "key-disabled", "key-deleted", "key-expired"} {
		t.Run(cause, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			var err error
			switch cause {
			case "user-disabled":
				_, err = integrationDB.Exec(`UPDATE users SET status='inactive' WHERE id=$1`, f.user.ID)
			case "user-deleted":
				_, err = integrationDB.Exec(`UPDATE users SET deleted_at=NOW() WHERE id=$1`, f.user.ID)
			case "key-disabled":
				_, err = integrationDB.Exec(`UPDATE api_keys SET status='inactive' WHERE id=$1`, f.key.ID)
			case "key-deleted":
				_, err = integrationDB.Exec(`UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, f.key.ID)
			case "key-expired":
				_, err = integrationDB.Exec(`UPDATE api_keys SET expires_at=NOW()-INTERVAL '1 minute' WHERE id=$1`, f.key.ID)
			}
			require.NoError(t, err)
			_, err = f.ledger.ReserveUpstreamRejection(context.Background(), f.command())
			require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
			assertRejectionMoney(t, f, "10", "0", "0")
		})
	}
}

func TestUpstreamRejectionAcceptedCallStillSettlesAfterKeySoftDelete(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := integrationDB.Exec(`UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	require.NoError(t, err)
	require.True(t, result.Applied)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
}

func TestUpstreamRejectionPendingRecoveryHasNoTTLRelease(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown,
	})
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET created_at=$2,updated_at=$2 WHERE request_id=$1`, c.RequestID, time.Now().AddDate(-1, 0, 0))
	require.NoError(t, err)
	pending, err := f.ledger.ListPendingUpstreamRejections(context.Background(), 100)
	require.NoError(t, err)
	found := false
	for _, row := range pending {
		if row.RequestID == c.RequestID {
			found = true
			require.Equal(t, service.UpstreamRejectionUnknown, row.State)
		}
	}
	require.True(t, found, "unknown survives expiry and remains available to reconciliation")
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
}

func TestUpstreamRejectionPendingPaginationPassesUnknownPrefix(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	// Scope writes to this fixture while making its four rows precede any older
	// fixtures in this shared integration database. No global rows are rewritten.
	var oldest time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT COALESCE(MIN(updated_at),NOW())-INTERVAL '1 hour' FROM upstream_rejection_reservations WHERE state IN ('reserved','sent','unknown')`).Scan(&oldest))
	requests := make([]*service.UpstreamRejectionReserveCommand, 4)
	for i := range requests {
		c := f.command()
		if i == len(requests)-1 {
			c.Model = "grok-imagine-video-1.5"
		}
		reserveAndSendRejection(t, f, c)
		_, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown,
		})
		require.NoError(t, err)
		requests[i] = c
	}
	target := requests[len(requests)-1]
	require.NoError(t, f.ledger.BindUpstreamRejectionTask(ctx, &service.UpstreamRejectionTaskBinding{
		RequestID: target.RequestID, APIKeyID: target.APIKeyID, TaskID: "paged-video-" + uuid.NewString(),
		DurationSeconds: 8, Resolution: "480p", OriginalModel: target.Model, UpstreamModel: target.Model,
	}))
	for i, c := range requests {
		_, err := integrationDB.Exec(`UPDATE upstream_rejection_reservations SET updated_at=$2 WHERE request_id=$1`, c.RequestID, oldest.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
	}
	query := &service.UpstreamRejectionPendingQuery{Limit: 2}
	seen := make(map[string]bool)
	found, pages := false, 0
	for pages < 4 && !found {
		page, err := f.ledger.ListPendingUpstreamRejectionsPage(ctx, query)
		require.NoError(t, err)
		require.NotNil(t, page)
		pages++
		for _, row := range page.Items {
			require.False(t, seen[row.RequestID], "a stable scan must advance past its cursor")
			seen[row.RequestID] = true
			if row.RequestID == target.RequestID {
				found = true
				require.NotNil(t, row.Task)
			}
		}
		if !found {
			require.NotNil(t, page.Next, "pending prefix must not truncate the recovery scan")
			query.After = page.Next
		}
	}
	require.True(t, found)
	require.Equal(t, 2, pages)
	for _, c := range requests {
		require.True(t, seen[c.RequestID])
	}
	filtered, err := f.ledger.ListPendingUpstreamRejectionsPage(ctx, &service.UpstreamRejectionPendingQuery{Limit: 2, TasksOnly: true})
	require.NoError(t, err)
	require.NotEmpty(t, filtered.Items)
	require.Equal(t, target.RequestID, filtered.Items[0].RequestID, "TasksOnly must skip all three taskless unknowns before applying LIMIT")
	for _, row := range filtered.Items {
		require.NotNil(t, row.Task)
	}
	assertRejectionMoney(t, f, "9.6", "0.4", "0")
}

func TestUpstreamRejectionNormalInvalidAmountsCannotReleaseHold(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	fields := map[string]func(*service.UsageBillingCommand, float64){
		"balance":       func(c *service.UsageBillingCommand, v float64) { c.BalanceCost = v },
		"key-quota":     func(c *service.UsageBillingCommand, v float64) { c.APIKeyQuotaCost = v },
		"key-window":    func(c *service.UsageBillingCommand, v float64) { c.APIKeyRateLimitCost = v },
		"account-quota": func(c *service.UsageBillingCommand, v float64) { c.AccountQuotaCost = v },
	}
	invalid := map[string]float64{
		"negative": -0.1, "subprecision-negative": -0.000000001,
		"nan": math.NaN(), "positive-infinity": math.Inf(1), "negative-infinity": math.Inf(-1),
	}
	assertInvalid := func(t *testing.T, mutate func(*service.UsageBillingCommand)) {
		t.Helper()
		bill := &service.UsageBillingCommand{
			RequestID: "invalid-normal-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
			AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
			BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
			RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
		}
		mutate(bill)
		_, err := f.repo.Apply(context.Background(), bill)
		require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
		assertRejectionMoney(t, f, "9.9", "0.1", "0")
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 0)
		var count int
		require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&count))
		require.Zero(t, count)
		stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
		require.NoError(t, err)
		require.Equal(t, service.UpstreamRejectionSent, stored.State)
	}
	for field, set := range fields {
		for name, amount := range invalid {
			t.Run(field+"/"+name, func(t *testing.T) {
				assertInvalid(t, func(bill *service.UsageBillingCommand) { set(bill, amount) })
			})
		}
	}
	for name, amount := range map[string]float64{
		"positive": 0.1, "negative": -0.1, "subprecision-positive": 0.000000001,
		"subprecision-negative": -0.000000001, "nan": math.NaN(), "positive-infinity": math.Inf(1), "negative-infinity": math.Inf(-1),
	} {
		t.Run("subscription/"+name, func(t *testing.T) {
			assertInvalid(t, func(bill *service.UsageBillingCommand) { bill.SubscriptionCost = amount })
		})
	}
}

func TestUpstreamRejectionCorruptSnapshotOrTaskCannotConsumeHold(t *testing.T) {
	for _, corruption := range []string{"snapshot-price", "snapshot-owner", "task-key"} {
		t.Run(corruption, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			c := f.command()
			c.Model = "grok-imagine-video-1.5"
			reserveAndSendRejection(t, f, c)
			binding := &service.UpstreamRejectionTaskBinding{
				RequestID: c.RequestID, APIKeyID: c.APIKeyID, TaskID: "integrity-video-" + uuid.NewString(),
				DurationSeconds: 8, Resolution: "480p", OriginalModel: c.Model, UpstreamModel: c.Model,
			}
			require.NoError(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding))
			var originalSnapshot, originalTask, originalFingerprint string
			require.NoError(t, integrationDB.QueryRow(`SELECT snapshot::text,task_metadata::text,fingerprint FROM upstream_rejection_reservations WHERE request_id=$1`, c.RequestID).Scan(&originalSnapshot, &originalTask, &originalFingerprint))
			// Restore exactly this fixture before another test performs a global
			// pending scan. No business row outside this fixture is touched.
			t.Cleanup(func() {
				_, err := integrationDB.Exec(`UPDATE upstream_rejection_reservations SET snapshot=$2::jsonb,task_metadata=$3::jsonb,fingerprint=$4 WHERE request_id=$1`, c.RequestID, originalSnapshot, originalTask, originalFingerprint)
				require.NoError(t, err)
			})
			changed := *c
			var err error
			switch corruption {
			case "snapshot-price":
				changed.Price = 0.2
				// A valid fingerprint for the changed snapshot must not excuse a
				// mismatch with the authoritative immutable price/identity columns.
				_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET snapshot=jsonb_set(snapshot,'{price}',to_jsonb($2::numeric)),fingerprint=$3 WHERE request_id=$1`, c.RequestID, changed.Price, changed.Fingerprint())
			case "snapshot-owner":
				changed.UserID++
				_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET snapshot=jsonb_set(snapshot,'{user_id}',to_jsonb($2::bigint)),fingerprint=$3 WHERE request_id=$1`, c.RequestID, changed.UserID, changed.Fingerprint())
			case "task-key":
				_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET task_metadata=jsonb_set(task_metadata,'{api_key_id}',to_jsonb($2::bigint)) WHERE request_id=$1`, c.RequestID, c.APIKeyID+1)
			}
			require.NoError(t, err)
			_, err = f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
			require.ErrorIs(t, err, service.ErrUpstreamRejectionIntegrity)
			_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
			require.ErrorIs(t, err, service.ErrUpstreamRejectionIntegrity)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
			var count int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, c.RequestID).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestUpstreamRejectionNormalCostAboveHoldReleasesExactlyOwnFunds(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	assertRejectionMoney(t, f, "9.9", "7.1", "0")
	bill := &service.UsageBillingCommand{
		RequestID: "normal-above-hold-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		BalanceCost: 0.35, APIKeyQuotaCost: 0.35, APIKeyRateLimitCost: 0.35,
		RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
	}
	for i := 0; i < 2; i++ {
		result, err := f.repo.Apply(context.Background(), bill)
		require.NoError(t, err)
		require.Equal(t, i == 0, result.Applied)
		assertRejectionMoney(t, f, "9.65", "7", "0.35")
	}
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 1)
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
}

func TestUpstreamRejectionNormalPayloadMismatchCannotReleaseHold(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	for _, hash := range []string{"", strings.Repeat("c", 64)} {
		t.Run(fmt.Sprintf("hash-length-%d", len(hash)), func(t *testing.T) {
			bill := &service.UsageBillingCommand{
				RequestID: "normal-wrong-payload-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
				AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
				BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
				RequestPayloadHash: hash, UpstreamRejectionReservationID: c.RequestID,
			}
			_, err := f.repo.Apply(context.Background(), bill)
			require.ErrorIs(t, err, service.ErrUpstreamRejectionConflict)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 0)
			var claims int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&claims))
			require.Zero(t, claims)
			stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
			require.NoError(t, err)
			require.Equal(t, service.UpstreamRejectionSent, stored.State)
		})
	}
}

func TestUpstreamRejectionReleaseFailureRollsBackAndResponseLossReplayIsSafe(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	release := &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionReleased,
		ConfirmedNoCharge: true,
	}
	removeFailure := failRejectionEvent(t, c.RequestID, service.UpstreamRejectionReleased)
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), release)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "7.1", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 0)
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionSent, stored.State)
	removeFailure()
	first, err := f.ledger.FinalizeUpstreamRejection(context.Background(), release)
	require.NoError(t, err)
	require.True(t, first.Applied)
	// Simulate the successful response being lost: a new process repeats the
	// exact finalization without knowing whether the first transaction committed.
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	replay, err := rebuilt.FinalizeUpstreamRejection(context.Background(), release)
	require.NoError(t, err)
	require.False(t, replay.Applied)
	assertRejectionMoney(t, f, "10", "7", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
}

func TestUpstreamRejectionVideoQuotePersistsIncludingExplicitZero(t *testing.T) {
	for _, prices := range []struct {
		name        string
		base, final float64
	}{{"paid", 0.08, 0.016}, {"explicit-zero", 0, 0}, {"free-final", 0.08, 0}} {
		t.Run(prices.name, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			c := f.command()
			c.Model = "grok-imagine-video-1.5"
			reserveAndSendRejection(t, f, c)
			base, final := prices.base, prices.final
			binding := &service.UpstreamRejectionTaskBinding{
				RequestID: c.RequestID, APIKeyID: c.APIKeyID, TaskID: "quoted-video-" + uuid.NewString(),
				DurationSeconds: 8, Resolution: "480p", OriginalModel: c.Model, UpstreamModel: c.Model,
				VideoBasePricePerSecond: &base, VideoFinalPricePerSecond: &final,
			}
			require.NoError(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding))
			require.NoError(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding))
			rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
			stored, err := rebuilt.GetUpstreamRejectionByTask(context.Background(), f.user.ID, f.key.ID, binding.TaskID)
			require.NoError(t, err)
			require.Equal(t, binding, stored.Task)
			require.NotNil(t, stored.Task.VideoBasePricePerSecond)
			require.NotNil(t, stored.Task.VideoFinalPricePerSecond)
			require.Equal(t, prices.base, *stored.Task.VideoBasePricePerSecond)
			require.Equal(t, prices.final, *stored.Task.VideoFinalPricePerSecond)
			changed := *binding
			differentPrice := prices.final + 0.1
			changed.VideoFinalPricePerSecond = &differentPrice
			require.ErrorIs(t, rebuilt.BindUpstreamRejectionTask(context.Background(), &changed), service.ErrUpstreamRejectionConflict)
			unchanged, err := rebuilt.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
			require.NoError(t, err)
			require.Equal(t, binding, unchanged.Task)
			assertRejectionReceiptCount(t, c, "task_bound", 1)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
		})
	}
}

func TestUpstreamRejectionInvalidVideoQuotesNeverBind(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	c.Model = "grok-imagine-video-1.5"
	reserveAndSendRejection(t, f, c)
	assertInvalid := func(t *testing.T, base, final *float64) {
		t.Helper()
		binding := &service.UpstreamRejectionTaskBinding{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, TaskID: "invalid-quote-" + uuid.NewString(),
			DurationSeconds: 8, Resolution: "480p", OriginalModel: c.Model, UpstreamModel: c.Model,
			VideoBasePricePerSecond: base, VideoFinalPricePerSecond: final,
		}
		require.ErrorIs(t, f.ledger.BindUpstreamRejectionTask(context.Background(), binding), service.ErrUpstreamRejectionInvalid)
		stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
		require.NoError(t, err)
		require.Nil(t, stored.Task)
		require.Equal(t, service.UpstreamRejectionSent, stored.State)
		assertRejectionReceiptCount(t, c, "task_bound", 0)
		assertRejectionMoney(t, f, "9.9", "0.1", "0")
	}
	valid := 0.08
	for name, amount := range map[string]float64{
		"negative": -0.1, "subprecision-negative": -0.000000001,
		"nan": math.NaN(), "positive-infinity": math.Inf(1), "negative-infinity": math.Inf(-1),
	} {
		t.Run("base/"+name, func(t *testing.T) { assertInvalid(t, &amount, &valid) })
		t.Run("final/"+name, func(t *testing.T) { assertInvalid(t, &valid, &amount) })
	}
	t.Run("base-only", func(t *testing.T) { assertInvalid(t, &valid, nil) })
	t.Run("final-only", func(t *testing.T) { assertInvalid(t, nil, &valid) })
	zero := 0.0
	t.Run("zero-base-only", func(t *testing.T) { assertInvalid(t, &zero, nil) })
	t.Run("zero-final-only", func(t *testing.T) { assertInvalid(t, nil, &zero) })
}

func TestUpstreamRejectionRepeatedUnknownObservationPreservesFirstEvidence(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	first := &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown,
		Evidence: service.UpstreamRejectionBillingEvidence{
			OriginalStatus: 500, UpstreamRequestID: "poll-a-" + uuid.NewString(), ResponseDigest: strings.Repeat("a", 64),
		},
	}
	result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), first)
	require.NoError(t, err)
	require.True(t, result.Applied)
	second := *first
	second.Evidence.OriginalStatus = 502
	second.Evidence.UpstreamRequestID = "poll-b-" + uuid.NewString()
	second.Evidence.ResponseDigest = strings.Repeat("b", 64)
	result, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &second)
	require.NoError(t, err)
	require.False(t, result.Applied, "another unknown poll is an observation, not a new money event")
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionUnknown, stored.State)
	require.Equal(t, first.Evidence, stored.Evidence)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionUnknown, 1)
	finalize := rejectedBillingCommand(c)
	for i := 0; i < 2; i++ {
		result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
		require.NoError(t, err)
		require.Equal(t, i == 0, result.Applied)
	}
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
	changedTerminal := *finalize
	changedTerminal.Evidence.UpstreamRequestID = "changed-terminal-" + uuid.NewString()
	_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &changedTerminal)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionConflict, "unknown observation tolerance must not weaken terminal idempotency")
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
}

func rejectionProjectionLog(c *service.UpstreamRejectionReserveCommand) *service.UsageLog {
	return &service.UsageLog{
		UserID: c.UserID, APIKeyID: c.APIKeyID, AccountID: c.AccountID, GroupID: &c.GroupID,
		RequestID: c.RequestID, Model: c.Model, RequestedModel: c.Model,
		TotalCost: c.Price, ActualCost: c.Price, RateMultiplier: 1,
		BillingType: service.BillingTypeBalance, CreatedAt: time.Now().UTC(),
		BillingSnapshot: &service.UsageBillingSnapshot{
			Version: 1, ChargeKind: "upstream_rejection", FixedCharge: c.Price,
			OriginalStatus: 403, RejectionRuleVersion: c.RuleVersion,
		},
	}
}

func TestUpstreamRejectionFixtureCleansOnlyOwnedRows(t *testing.T) {
	ctx := context.Background()
	preserved := newRejectionBillingFixture(t, 10)
	preservedRequest := preserved.command()
	preservedLog := rejectionProjectionLog(preservedRequest)
	_, err := NewUsageLogRepository(testEntClient(t), integrationDB).Create(ctx, preservedLog)
	require.NoError(t, err)
	var owned rejectionBillingFixture
	var requestID, additionalKey string
	t.Run("owned-fixture", func(t *testing.T) {
		owned = newRejectionBillingFixture(t, 10)
		additionalKey = "test-rejection-cleanup-" + uuid.NewString()
		key := mustCreateApiKey(t, testEntClient(t), &service.APIKey{
			UserID: owned.user.ID, GroupID: &owned.group.ID, Key: additionalKey, Name: "extra cleanup key",
		})
		c := owned.command()
		c.APIKeyID = key.ID
		requestID = c.RequestID
		reserveAndSendRejection(t, owned, c)
		_, err := owned.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(c))
		require.NoError(t, err)
		usage := rejectionProjectionLog(c)
		// An interrupted negative projection test can leave incorrect attribution.
		// Its own key still identifies it; cleanup must preserve the other row.
		usage.UserID, usage.AccountID, usage.GroupID = preserved.user.ID, preserved.account.ID, &preserved.group.ID
		_, err = NewUsageLogRepository(testEntClient(t), integrationDB).Create(ctx, usage)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `INSERT INTO scheduler_outbox(event_type,account_id,group_id) VALUES('account_changed',$1,$2)`, owned.account.ID, owned.group.ID)
		require.NoError(t, err)
	})
	for _, check := range []struct {
		query string
		args  []any
	}{
		{`SELECT count(*) FROM users WHERE id=$1`, []any{owned.user.ID}},
		{`SELECT count(*) FROM api_keys WHERE user_id=$1`, []any{owned.user.ID}},
		{`SELECT count(*) FROM accounts WHERE id=$1`, []any{owned.account.ID}},
		{`SELECT count(*) FROM groups WHERE id=$1`, []any{owned.group.ID}},
		{`SELECT count(*) FROM usage_logs WHERE request_id=$1`, []any{requestID}},
		{`SELECT count(*) FROM upstream_rejection_reservations WHERE request_id=$1`, []any{requestID}},
		{`SELECT count(*) FROM upstream_rejection_billing_events WHERE request_id=$1`, []any{requestID}},
		{`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, []any{requestID}},
		{`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 OR group_id=$2`, []any{owned.account.ID, owned.group.ID}},
		{`SELECT count(*) FROM auth_cache_invalidation_outbox WHERE cache_key=encode(sha256(convert_to($1,'UTF8')),'hex')`, []any{additionalKey}},
	} {
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx, check.query, check.args...).Scan(&count))
		require.Zero(t, count, check.query)
	}
	var preservedCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE id=$1 AND user_id=$2 AND api_key_id=$3`, preservedLog.ID, preserved.user.ID, preserved.key.ID).Scan(&preservedCount))
	require.Equal(t, 1, preservedCount, "fixture cleanup must not delete another test's usage")
}

func TestUpstreamRejectionProjectionOutboxSurvivesCrashAndPagesTextReceipts(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	var oldest time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT COALESCE(MIN(updated_at),NOW())-INTERVAL '1 hour' FROM upstream_rejection_reservations WHERE state='rejected' AND usage_projected_at IS NULL`).Scan(&oldest))
	requests := make([]*service.UpstreamRejectionReserveCommand, 4)
	for i := range requests {
		c := f.command()
		reserveAndSendRejection(t, f, c)
		_, err := f.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(c))
		require.NoError(t, err)
		// Deliberately omit usage.Create: the process crashed after charging but
		// before producing the user-facing projection. These are all text calls.
		_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET updated_at=$2 WHERE request_id=$1`, c.RequestID, oldest.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		requests[i] = c
	}
	rebuilt := NewUsageBillingRepository(testEntClient(t), integrationDB).(service.UpstreamRejectionBillingRepository)
	target := requests[len(requests)-1]
	query := &service.UpstreamRejectionPendingQuery{Limit: 2, TasksOnly: true}
	seen := make(map[string]bool)
	for pages := 0; pages < 2; pages++ {
		page, err := rebuilt.ListUnprojectedUpstreamRejectionsPage(ctx, query)
		require.NoError(t, err)
		require.Len(t, page.Items, 2)
		for _, row := range page.Items {
			require.Nil(t, row.Task, "usage projection must also recover text calls without provider tasks")
			require.Nil(t, row.UsageProjectedAt)
			require.False(t, seen[row.RequestID])
			seen[row.RequestID] = true
		}
		require.NotNil(t, page.Next)
		query.After = page.Next
	}
	for _, c := range requests {
		require.True(t, seen[c.RequestID], "a durable prefix must not hide later projection work")
	}
	assertRejectionMoney(t, f, "9.6", "0", "0.4")
	require.ErrorIs(t, rebuilt.MarkUpstreamRejectionUsageProjected(ctx, target.RequestID, target.APIKeyID), service.ErrUpstreamRejectionIntegrity)
	usage := rejectionProjectionLog(target)
	created, err := NewUsageLogRepository(testEntClient(t), integrationDB).Create(ctx, usage)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, rebuilt.MarkUpstreamRejectionUsageProjected(ctx, target.RequestID, target.APIKeyID))
	// The acknowledgement can itself be retried after its response is lost.
	require.NoError(t, rebuilt.MarkUpstreamRejectionUsageProjected(ctx, target.RequestID, target.APIKeyID))
	stored, err := rebuilt.GetUpstreamRejection(ctx, target.RequestID, target.APIKeyID)
	require.NoError(t, err)
	require.NotNil(t, stored.UsageProjectedAt)
	require.True(t, stored.UpdatedAt.Equal(oldest.Add(3*time.Second)), "ack must not reorder the reconciliation cursor")
	var pending, claims, events int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_reservations WHERE request_id=$1 AND usage_projected_at IS NULL`, target.RequestID).Scan(&pending))
	require.Zero(t, pending)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, target.RequestID, target.APIKeyID).Scan(&claims))
	require.Equal(t, 1, claims)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_billing_events WHERE request_id=$1`, target.RequestID).Scan(&events))
	require.Equal(t, 3, events, "reserve/sent/rejected only; projection cannot add money events")
	assertRejectionMoney(t, f, "9.6", "0", "0.4")
}

func TestUpstreamRejectionProjectionAcknowledgementRequiresMatchingUsage(t *testing.T) {
	for _, invalid := range []string{"actual-amount", "total-amount", "user", "account", "group", "charge-kind"} {
		t.Run(invalid, func(t *testing.T) {
			f, other := newRejectionBillingFixture(t, 10), newRejectionBillingFixture(t, 10)
			c := f.command()
			reserveAndSendRejection(t, f, c)
			_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
			require.NoError(t, err)
			require.ErrorIs(t, f.ledger.MarkUpstreamRejectionUsageProjected(context.Background(), c.RequestID, c.APIKeyID), service.ErrUpstreamRejectionIntegrity)
			usage := rejectionProjectionLog(c)
			switch invalid {
			case "actual-amount":
				usage.ActualCost = 0.2
			case "total-amount":
				usage.TotalCost = 0.2
			case "user":
				usage.UserID = other.user.ID
			case "account":
				usage.AccountID = other.account.ID
			case "group":
				usage.GroupID = &other.group.ID
			case "charge-kind":
				usage.BillingSnapshot.ChargeKind = "ordinary"
			}
			created, err := NewUsageLogRepository(testEntClient(t), integrationDB).Create(context.Background(), usage)
			require.NoError(t, err)
			require.True(t, created)
			require.ErrorIs(t, f.ledger.MarkUpstreamRejectionUsageProjected(context.Background(), c.RequestID, c.APIKeyID), service.ErrUpstreamRejectionIntegrity)
			stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
			require.NoError(t, err)
			require.Nil(t, stored.UsageProjectedAt)
			// Repair only this synthetic projection row; do not repeat any charge.
			_, err = integrationDB.Exec(`UPDATE usage_logs SET user_id=$2,account_id=$3,group_id=$4,total_cost=$5,actual_cost=$5,billing_snapshot=jsonb_set(billing_snapshot,'{charge_kind}','"upstream_rejection"'::jsonb) WHERE id=$1 AND request_id=$6 AND api_key_id=$7`, usage.ID, c.UserID, c.AccountID, c.GroupID, c.Price, c.RequestID, c.APIKeyID)
			require.NoError(t, err)
			require.NoError(t, f.ledger.MarkUpstreamRejectionUsageProjected(context.Background(), c.RequestID, c.APIKeyID))
			require.NoError(t, f.ledger.MarkUpstreamRejectionUsageProjected(context.Background(), c.RequestID, c.APIKeyID))
			assertRejectionMoney(t, f, "9.9", "0", "0.1")
			assertRejectionMoney(t, other, "10", "0", "0")
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
			var claims int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, c.RequestID, c.APIKeyID).Scan(&claims))
			require.Equal(t, 1, claims)
		})
	}
}

func TestUpstreamRejectionMarkSentAndStaleReservedReleaseRace(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	c := f.command()
	_, err = f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET created_at=NOW()-INTERVAL '10 minutes',updated_at=NOW()-INTERVAL '10 minutes' WHERE request_id=$1`, c.RequestID)
	require.NoError(t, err)
	type raceResult struct {
		result *service.UpstreamRejectionBillingResult
		err    error
	}
	results := make(chan raceResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		result, err := f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID)
		results <- raceResult{result, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionReleased,
			ConfirmedNoCharge: false,
		})
		results <- raceResult{result, err}
	}()
	close(start)
	wg.Wait()
	close(results)
	applied, stateErrors := 0, 0
	for outcome := range results {
		if outcome.err == nil {
			require.NotNil(t, outcome.result)
			require.True(t, outcome.result.Applied)
			applied++
		} else {
			require.ErrorIs(t, outcome.err, service.ErrUpstreamRejectionState)
			stateErrors++
		}
	}
	require.Equal(t, 1, applied)
	require.Equal(t, 1, stateErrors)
	stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	switch stored.State {
	case service.UpstreamRejectionSent:
		assertRejectionMoney(t, f, "9.9", "7.1", "0")
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionSent, 1)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 0)
	case service.UpstreamRejectionReleased:
		assertRejectionMoney(t, f, "10", "7", "0")
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionSent, 0)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
	default:
		t.Fatalf("unexpected race winner state: %s", stored.State)
	}
}

func rejectionPlatformQuotaJSON(t *testing.T, id int64) string {
	t.Helper()
	var row string
	require.NoError(t, integrationDB.QueryRow(`SELECT row_to_json(q)::text FROM user_platform_quotas q WHERE id=$1`, id).Scan(&row))
	return row
}

func TestUpstreamRejectionConfiguredGrokPlatformQuotasFailBeforeMoney(t *testing.T) {
	for _, column := range []string{"daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd"} {
		for _, limit := range []float64{0, 1} {
			t.Run(fmt.Sprintf("%s/%g", column, limit), func(t *testing.T) {
				f := newRejectionBillingFixture(t, 10)
				_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
				require.NoError(t, err)
				var quotaID int64
				require.NoError(t, integrationDB.QueryRow(`INSERT INTO user_platform_quotas(user_id,platform,`+pq.QuoteIdentifier(column)+`,daily_usage_usd,weekly_usage_usd,monthly_usage_usd) VALUES($1,'grok',$2,0.01,0.02,0.03) RETURNING id`, f.user.ID, limit).Scan(&quotaID))
				before := rejectionPlatformQuotaJSON(t, quotaID)
				c := f.command()
				_, err = f.ledger.ReserveUpstreamRejection(context.Background(), c)
				require.ErrorIs(t, err, service.ErrUpstreamRejectionPlatformQuotaUnsupported)
				assertRejectionMoney(t, f, "10", "7", "0")
				var holds, events int
				require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_reservations WHERE request_id=$1`, c.RequestID).Scan(&holds))
				require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_billing_events WHERE request_id=$1`, c.RequestID).Scan(&events))
				require.Zero(t, holds)
				require.Zero(t, events)
				require.Equal(t, before, rejectionPlatformQuotaJSON(t, quotaID), "admission must not silently clear configured platform limits or usage")
			})
		}
	}
}

func TestUpstreamRejectionUnrelatedOrUnconfiguredPlatformQuotaAllowsAdmission(t *testing.T) {
	for _, scenario := range []string{"other-platform", "all-null", "soft-deleted"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			platform := "grok"
			var limit, deleted any
			switch scenario {
			case "other-platform":
				platform, limit = "openai", 0
			case "soft-deleted":
				limit, deleted = 0, time.Now().UTC()
			}
			var quotaID int64
			require.NoError(t, integrationDB.QueryRow(`INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,weekly_limit_usd,monthly_limit_usd,deleted_at) VALUES($1,$2,$3,$3,$3,$4) RETURNING id`, f.user.ID, platform, limit, deleted).Scan(&quotaID))
			before := rejectionPlatformQuotaJSON(t, quotaID)
			c := f.command()
			result, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
			require.NoError(t, err)
			require.True(t, result.Applied)
			assertRejectionMoney(t, f, "9.9", "0.1", "0")
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionReserved, 1)
			require.Equal(t, before, rejectionPlatformQuotaJSON(t, quotaID))
		})
	}
}

func TestUpstreamRejectionPlatformQuotaAddedAfterAdmissionDoesNotErasePromise(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	var quotaID int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,weekly_limit_usd,monthly_limit_usd) VALUES($1,'grok',0,0,0) RETURNING id`, f.user.ID).Scan(&quotaID))
	before := rejectionPlatformQuotaJSON(t, quotaID)
	finalize := rejectedBillingCommand(c)
	for i := 0; i < 2; i++ {
		result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
		require.NoError(t, err)
		require.Equal(t, i == 0, result.Applied)
	}
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
	_, err := f.ledger.ReserveUpstreamRejection(context.Background(), f.command())
	require.ErrorIs(t, err, service.ErrUpstreamRejectionPlatformQuotaUnsupported)
	require.Equal(t, before, rejectionPlatformQuotaJSON(t, quotaID), "existing promises settle without rewriting the newly configured limit")
}

func TestUpstreamRejectionZeroSupplierCostChargesCustomerWithoutSupplierQuota(t *testing.T) {
	for _, negativeZero := range []bool{false, true} {
		t.Run(fmt.Sprintf("negative-zero=%t", negativeZero), func(t *testing.T) {
			f := newRejectionBillingFixture(t, 10)
			_, err := integrationDB.Exec(`UPDATE accounts SET extra='{"quota_limit":100,"quota_used":4.2,"quota_daily_limit":50,"quota_daily_used":2.3,"quota_weekly_limit":80,"quota_weekly_used":3.4}'::jsonb WHERE id=$1`, f.account.ID)
			require.NoError(t, err)
			var before string
			require.NoError(t, integrationDB.QueryRow(`SELECT extra::text FROM accounts WHERE id=$1`, f.account.ID).Scan(&before))
			c := f.command()
			c.UpstreamCost = 0
			if negativeZero {
				c.UpstreamCost = math.Copysign(0, -1)
				require.True(t, math.Signbit(c.UpstreamCost))
			}
			reserveAndSendRejection(t, f, c)
			require.Equal(t, negativeZero, math.Signbit(c.UpstreamCost), "canonicalization must not mutate the caller's reusable command")
			replayed, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
			require.NoError(t, err)
			require.False(t, replayed.Applied)
			stored, err := f.ledger.GetUpstreamRejection(context.Background(), c.RequestID, c.APIKeyID)
			require.NoError(t, err, "negative zero must survive JSONB canonicalization without a fingerprint mismatch")
			require.Zero(t, stored.UpstreamCost)
			require.False(t, math.Signbit(stored.UpstreamCost))
			finalize := rejectedBillingCommand(c)
			for i := 0; i < 2; i++ {
				result, err := f.ledger.FinalizeUpstreamRejection(context.Background(), finalize)
				require.NoError(t, err)
				require.Equal(t, i == 0, result.Applied)
			}
			assertRejectionMoney(t, f, "9.9", "0", "0.1")
			var after, cost string
			require.NoError(t, integrationDB.QueryRow(`SELECT extra::text FROM accounts WHERE id=$1`, f.account.ID).Scan(&after))
			require.Equal(t, before, after, "zero supplier cost must not increment/reset supplier quota counters")
			require.NoError(t, integrationDB.QueryRow(`SELECT snapshot->>'upstream_cost' FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind='rejected'`, c.RequestID).Scan(&cost))
			amount, err := decimal.NewFromString(cost)
			require.NoError(t, err)
			require.True(t, amount.IsZero())
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
		})
	}
}

func rejectionUserPendingFixtureState(t *testing.T, f rejectionBillingFixture, state string, createdAt time.Time) *service.UpstreamRejectionReserveCommand {
	t.Helper()
	c := f.command()
	_, err := f.ledger.ReserveUpstreamRejection(context.Background(), c)
	require.NoError(t, err)
	if state != service.UpstreamRejectionReserved && state != service.UpstreamRejectionReleased {
		_, err = f.ledger.MarkUpstreamRejectionSent(context.Background(), c.RequestID, c.APIKeyID)
		require.NoError(t, err)
	}
	switch state {
	case service.UpstreamRejectionReserved, service.UpstreamRejectionSent:
	case service.UpstreamRejectionUnknown:
		_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: state,
			Evidence: service.UpstreamRejectionBillingEvidence{
				OriginalStatus: 500, UpstreamRequestID: "private-upstream-id", ResponseDigest: strings.Repeat("e", 64),
			},
		})
	case service.UpstreamRejectionRejected:
		_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), rejectedBillingCommand(c))
	case service.UpstreamRejectionReleased:
		_, err = f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: state,
		})
	case service.UpstreamRejectionNormal:
		_, err = f.repo.Apply(context.Background(), &service.UsageBillingCommand{
			RequestID: "public-pending-normal-" + uuid.NewString(), UserID: c.UserID, APIKeyID: c.APIKeyID,
			AccountID: c.AccountID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
			BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
			RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
		})
	default:
		t.Fatalf("unsupported fixture state: %s", state)
	}
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET created_at=$2 WHERE request_id=$1`, c.RequestID, createdAt)
	require.NoError(t, err)
	return c
}

func TestUpstreamRejectionUserPendingOwnedStatesPaginationAndPrivacy(t *testing.T) {
	f, other := newRejectionBillingFixture(t, 10), newRejectionBillingFixture(t, 10)
	reader, ok := f.repo.(service.UserPendingUpstreamRejectionReader)
	require.True(t, ok)
	base := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	states := []string{service.UpstreamRejectionReserved, service.UpstreamRejectionSent, service.UpstreamRejectionUnknown,
		service.UpstreamRejectionReserved, service.UpstreamRejectionUnknown,
		service.UpstreamRejectionRejected, service.UpstreamRejectionReleased, service.UpstreamRejectionNormal}
	type expectedRow struct {
		requestID string
		state     string
		createdAt time.Time
	}
	expected := make([]expectedRow, 0, 5)
	foreignIDs := make(map[string]bool)
	for i, state := range states {
		// Several rows deliberately share a timestamp; request_id is the
		// deterministic tie breaker and must be part of the descending cursor.
		createdAt := base.Add(time.Duration(i/3) * time.Second)
		c := rejectionUserPendingFixtureState(t, f, state, createdAt)
		foreign := rejectionUserPendingFixtureState(t, other, state, createdAt)
		foreignIDs[foreign.RequestID] = true
		if i < 5 {
			expected = append(expected, expectedRow{c.RequestID, state, createdAt})
		}
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].createdAt.Equal(expected[j].createdAt) {
			return expected[i].requestID > expected[j].requestID
		}
		return expected[i].createdAt.After(expected[j].createdAt)
	})
	var actual []service.UserPendingUpstreamRejection
	seen := make(map[string]bool)
	cursor := ""
	for pageNumber := 0; pageNumber < 4; pageNumber++ {
		page, err := reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, cursor, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 2)
		for _, row := range page.Items {
			require.False(t, foreignIDs[row.RequestID])
			require.False(t, seen[row.RequestID], "cursor must not duplicate timestamp ties")
			seen[row.RequestID] = true
			actual = append(actual, row)
		}
		encoded, err := json.Marshal(page)
		require.NoError(t, err)
		var public map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(encoded, &public))
		for field := range public {
			require.Contains(t, []string{"items", "next_cursor"}, field)
		}
		var items []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(public["items"], &items))
		for _, item := range items {
			require.Len(t, item, 5)
			for field := range item {
				require.Contains(t, []string{"request_id", "model", "state", "held_price", "created_at"}, field)
			}
		}
		for _, private := range []string{"upstream_cost", "upstream_identity_hash", "request_payload_hash", "api_key_id", "account_id", "evidence", "private-upstream-id", strings.Repeat("d", 64), strings.Repeat("a", 64)} {
			require.NotContains(t, string(encoded), private)
		}
		if page.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, page.NextCursor)
		cursor = page.NextCursor
	}
	require.Len(t, actual, len(expected), "every own pending row appears once; terminal rows never appear")
	for i, want := range expected {
		require.Equal(t, want.requestID, actual[i].RequestID)
		require.Equal(t, want.state, actual[i].State)
		require.True(t, want.createdAt.Equal(actual[i].CreatedAt))
		require.Equal(t, "grok-4.7", actual[i].Model)
		require.Equal(t, 0.1, actual[i].HeldPrice)
	}
	assertRejectionMoney(t, f, "9.375", "0.5", "0.125")
	assertRejectionMoney(t, other, "9.375", "0.5", "0.125")
}

func TestUpstreamRejectionUserPendingCursorCannotAuthorizeAnotherOwner(t *testing.T) {
	f, other := newRejectionBillingFixture(t, 10), newRejectionBillingFixture(t, 10)
	reader := f.repo.(service.UserPendingUpstreamRejectionReader)
	ownIDs, foreignIDs := make(map[string]bool), make(map[string]bool)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		own := rejectionUserPendingFixtureState(t, f, service.UpstreamRejectionReserved, base.Add(time.Duration(i)*time.Second))
		foreign := rejectionUserPendingFixtureState(t, other, service.UpstreamRejectionReserved, base.Add(time.Duration(i)*time.Second))
		ownIDs[own.RequestID], foreignIDs[foreign.RequestID] = true, true
	}
	foreignPage, err := reader.ListUserPendingUpstreamRejections(context.Background(), other.user.ID, "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, foreignPage.NextCursor)
	_, err = reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, foreignPage.NextCursor, 50)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	payload, err := base64.RawURLEncoding.DecodeString(foreignPage.NextCursor)
	require.NoError(t, err)
	var forged map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &forged))
	var foreignRequestID string
	require.NoError(t, json.Unmarshal(forged["r"], &foreignRequestID))
	require.True(t, foreignIDs[foreignRequestID])
	// Cursor owner is not an authorization claim. Even after changing it to
	// match the authenticated caller, the foreign request ID grants no access.
	forged["u"], err = json.Marshal(f.user.ID)
	require.NoError(t, err)
	forged["t"], err = json.Marshal(base.Add(time.Hour))
	require.NoError(t, err)
	payload, err = json.Marshal(forged)
	require.NoError(t, err)
	page, err := reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, base64.RawURLEncoding.EncodeToString(payload), 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	for _, row := range page.Items {
		require.True(t, ownIDs[row.RequestID])
		require.False(t, foreignIDs[row.RequestID])
	}
}

func TestUpstreamRejectionUserPendingValidatesPageBoundsAndCursor(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	reader := f.repo.(service.UserPendingUpstreamRejectionReader)
	for _, size := range []int{-1, 0, 51, 1000} {
		t.Run(fmt.Sprintf("invalid-size-%d", size), func(t *testing.T) {
			_, err := reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, "", size)
			require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
		})
	}
	for _, size := range []int{1, 50} {
		page, err := reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, "", size)
		require.NoError(t, err)
		require.NotNil(t, page.Items)
		require.Empty(t, page.Items)
		require.Empty(t, page.NextCursor)
	}
	for _, cursor := range []string{"not-base64%", base64.RawURLEncoding.EncodeToString([]byte(`{}`)), strings.Repeat("a", 1025)} {
		_, err := reader.ListUserPendingUpstreamRejections(context.Background(), f.user.ID, cursor, 2)
		require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	}
	for _, owner := range []int64{0, -1} {
		_, err := reader.ListUserPendingUpstreamRejections(context.Background(), owner, "", 2)
		require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	}
}
