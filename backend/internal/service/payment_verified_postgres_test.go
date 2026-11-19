//go:build unit && integration

package service

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment/proof"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	pgcontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func verifiedPostgres(t *testing.T) *dbent.Client {
	t.Helper()
	ctx := context.Background()
	img := os.Getenv("SUB2API_TEST_POSTGRES_IMAGE")
	if img == "" {
		img = "postgres:18-alpine"
	}
	pg, err := pgcontainer.Run(ctx, img, pgcontainer.WithDatabase("verified_payment_test"), pgcontainer.WithUsername("test"), pgcontainer.WithPassword("test-only"), pgcontainer.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Schema.Create(ctx))
	raw, err := os.ReadFile("../../migrations/184_auth_cache_invalidation_outbox.sql")
	require.NoError(t, err)
	_, err = client.ExecContext(ctx, string(raw))
	require.NoError(t, err)
	return client
}

func TestVerifiedNativePostgresConcurrency(t *testing.T) {
	f := newVerifiedFixtureWithClient(t, "usdt_bsc", verifiedPostgres(t))
	f.state.Store(2)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.notify() }()
	}
	wg.Wait()
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
	r, err := proof.Load(context.Background(), f.svc.entClient, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, "100", r.Amount)
	row, err := f.svc.entClient.PaymentOrder.Get(context.Background(), f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, row.Status)
}

func TestVerifiedNativePostgresAuditRollback(t *testing.T) {
	f := newVerifiedFixtureWithClient(t, "usdt_bsc", verifiedPostgres(t))
	f.state.Store(2)
	ctx := context.Background()
	_, err := f.svc.entClient.ExecContext(ctx, `CREATE FUNCTION reject_verified_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='ORDER_PAID' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_verified BEFORE INSERT ON payment_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_verified_audit();`)
	require.NoError(t, err)
	require.Error(t, f.notify())
	require.Zero(t, f.credits.Load())
	_, err = proof.Load(ctx, f.svc.entClient, f.order.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	row, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, row.Status)
	require.Nil(t, row.PaidAt)
	_, err = f.svc.entClient.ExecContext(ctx, `DROP TRIGGER reject_verified ON payment_audit_logs`)
	require.NoError(t, err)
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
}

func TestVerifiedNativePostgresCommittedReceiptRecovery(t *testing.T) {
	f := newVerifiedFixtureWithClient(t, "usdt_bsc", verifiedPostgres(t))
	f.state.Store(2)
	ctx := context.Background()
	p, err := f.svc.getOrderProvider(ctx, f.order)
	require.NoError(t, err)
	q, err := p.QueryOrder(ctx, f.order.PaymentTradeNo)
	require.NoError(t, err)
	r, err := f.svc.paymentEvidence(ctx, f.order, p, q)
	require.NoError(t, err)
	tx, err := f.svc.entClient.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, proof.Save(ctx, tx.Client(), r))
	_, err = tx.PaymentOrder.UpdateOneID(f.order.ID).SetStatus(OrderStatusPaid).SetPaidAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, paymentAuditOnce(ctx, tx.Client(), f.order.ID, "ORDER_PAID", r.Evidence))
	require.NoError(t, tx.Commit())
	// A new process sees the committed receipt, not an in-memory callback.
	queries := f.calls.Load()
	restarted := *f.svc
	require.NoError(t, restarted.reconcileVerifiedPayments(ctx))
	require.Equal(t, int64(110), f.credits.Load())
	require.NoError(t, restarted.reconcileVerifiedPayments(ctx))
	require.Equal(t, int64(110), f.credits.Load())
	require.Equal(t, queries, f.calls.Load())
	require.NoError(t, restarted.reconcilePaymentPostCommit(ctx))
	require.True(t, restarted.hasAuditLog(ctx, f.order.ID, paymentPostCommitDone))
}
