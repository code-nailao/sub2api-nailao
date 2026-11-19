//go:build integration

// Package acceptance exercises the actual payment service and actual repositories.
// Only the upstream gateway is simulated; money and redeem codes live in PostgreSQL.
package acceptance

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	pgcontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type fixture struct {
	client   *dbent.Client
	db       *sql.DB
	dsn      string
	svc      *service.PaymentService
	webhook  http.Handler
	userID   int64
	order    *service.CreateOrderResponse
	state    atomic.Int32
	orderRef string
}

func realFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	image := os.Getenv("SUB2API_TEST_POSTGRES_IMAGE")
	if image == "" {
		image = "postgres:18-alpine"
	}
	pg, err := pgcontainer.Run(ctx, image, pgcontainer.WithDatabase("native_real_money"), pgcontainer.WithUsername("test"), pgcontainer.WithPassword("isolated-test"), pgcontainer.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	f := &fixture{dsn: dsn}
	f.open(t)
	require.NoError(t, f.client.Schema.Create(ctx))
	for _, path := range []string{"../../../migrations/184_auth_cache_invalidation_outbox.sql", "../../../migrations/243_payment_verified_receipts.sql"} {
		raw, e := os.ReadFile(path)
		require.NoError(t, e)
		_, e = f.db.ExecContext(ctx, string(raw))
		require.NoError(t, e)
	}
	_, err = f.db.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_audit_order_action ON payment_audit_logs(order_id,action)")
	require.NoError(t, err)
	f.state.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		if r.URL.Path == "/api/v1/order/create-transaction" {
			f.orderRef = in["order_id"].(string)
		} else if in["trade_id"] != "gateway_real_money" {
			http.Error(w, "unknown", 404)
			return
		}
		if r.URL.Path == "/api/v1/order/cancel-transaction" {
			f.state.Store(4)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "data": map[string]any{"trade_id": "gateway_real_money", "order_id": f.orderRef, "trade_type": "usdt.bep20", "fiat": "CNY", "money": "100.00", "amount": "100.00", "actual_amount": "14.250000000000000001", "token": "0x1111111111111111111111111111111111111111", "status": f.state.Load(), "expiration_time": 599, "expired_at": time.Now().Add(10 * time.Minute).Unix(), "payment_url": "/pay/checkout/gateway_real_money", "trade_url": "https://bscscan.com/tx/0x" + strings.Repeat("a", 64)}})
	}))
	t.Cleanup(server.Close)
	settings := repository.NewSettingRepository(f.client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingPaymentEnabled: "true", service.SettingOrderTimeoutMinutes: "10", service.SettingMaxPendingOrders: "10", service.SettingEnabledPaymentTypes: `["usdt_bsc"]`, service.SettingBalanceRechargeMult: "1", service.SettingMinRechargeAmount: "1"}))
	cfg := service.NewPaymentConfigService(f.client, settings, nil)
	inst, err := cfg.CreateProviderInstance(ctx, service.CreateProviderInstanceRequest{ProviderKey: "bepusdt", Name: "synthetic gateway", SupportedTypes: []string{"usdt_bsc"}, Config: map[string]string{"apiBase": server.URL, "apiToken": "test-only", "notifyUrl": "https://main.example/api/v1/payment/webhook/bepusdt", "returnUrl": "https://main.example/payment/result"}})
	require.NoError(t, err)
	require.False(t, inst.Enabled)
	_, err = f.client.PaymentProviderInstance.UpdateOneID(inst.ID).SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	u, err := f.client.User.Create().SetEmail("money@test.invalid").SetUsername("synthetic").SetPasswordHash("test-only").SetBalance(5).SetStatus("active").Save(ctx)
	require.NoError(t, err)
	f.userID = u.ID
	f.services()
	f.order, err = f.svc.CreateOrder(ctx, service.CreateOrderRequest{UserID: u.ID, Amount: 100, PaymentType: "usdt_bsc", OrderType: "balance", SrcHost: "main.example", ReturnURL: "https://untrusted.example/"})
	require.NoError(t, err)
	require.False(t, f.order.VerificationPending)
	require.Contains(t, f.order.PayURL, "/pay/checkout/gateway_real_money")
	t.Cleanup(func() { _ = f.client.Close() })
	return f
}

func (f *fixture) open(t *testing.T) {
	var err error
	f.db, err = sql.Open("postgres", f.dsn)
	require.NoError(t, err)
	f.client = dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, f.db)))
}
func (f *fixture) services() {
	users := repository.NewUserRepository(f.client, f.db)
	redeem := service.NewRedeemService(repository.NewRedeemCodeRepository(f.client), users, nil, nil, nil, f.client, nil, nil)
	cfg := service.NewPaymentConfigService(f.client, repository.NewSettingRepository(f.client), nil)
	reg := payment.NewRegistry()
	f.svc = service.NewPaymentService(f.client, reg, payment.NewDefaultLoadBalancer(f.client, nil), redeem, nil, cfg, users, nil, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/notify", handler.NewPaymentWebhookHandler(f.svc, reg).BEPusdtNotify)
	f.webhook = router
}
func (f *fixture) restart(t *testing.T) {
	require.NoError(t, f.client.Close())
	f.open(t)
	f.services()
}
func (f *fixture) balance(t *testing.T) string {
	var value string
	require.NoError(t, f.db.QueryRow("SELECT balance::text FROM users WHERE id=$1", f.userID).Scan(&value))
	return value
}
func (f *fixture) notify() *httptest.ResponseRecorder {
	m := map[string]any{"trade_id": "gateway_real_money", "order_id": f.orderRef, "status": 2, "amount": 100, "actual_amount": "14.250000000000000001", "token": "0x1111111111111111111111111111111111111111", "block_transaction_id": "0x" + strings.Repeat("a", 64)}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+fmt.Sprint(m[k]))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + "test-only"))
	m["signature"] = hex.EncodeToString(sum[:])
	body, _ := json.Marshal(m)
	req := httptest.NewRequest("POST", "/notify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.webhook.ServeHTTP(w, req)
	return w
}
func (f *fixture) assertSettled(t *testing.T) {
	t.Helper()
	require.Equal(t, "105.00000000", f.balance(t))
	o, err := f.client.PaymentOrder.Get(context.Background(), f.order.OrderID)
	require.NoError(t, err)
	require.Equal(t, service.OrderStatusCompleted, o.Status)
	var codes, receipts int
	require.NoError(t, f.db.QueryRow("SELECT count(*) FROM redeem_codes WHERE code=$1 AND status='used' AND used_by=$2 AND value=100", o.RechargeCode, f.userID).Scan(&codes))
	require.Equal(t, 1, codes)
	require.NoError(t, f.db.QueryRow("SELECT count(*) FROM payment_receipts WHERE order_id=$1", o.ID).Scan(&receipts))
	require.Equal(t, 1, receipts)
}

func TestNativeMoneyPostgresWebhookConcurrencyAndRestart(t *testing.T) {
	f := realFixture(t)
	require.NotEqual(t, "success", strings.TrimSpace(f.notify().Body.String()))
	require.Equal(t, "5.00000000", f.balance(t))
	f.state.Store(2)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.notify() }()
	}
	wg.Wait()
	f.restart(t)
	require.Equal(t, "success", strings.TrimSpace(f.notify().Body.String()))
	f.assertSettled(t)
	for i := 0; i < 3; i++ {
		_, err := f.svc.ReconcilePendingPaymentOrders(context.Background())
		require.NoError(t, err)
	}
	f.assertSettled(t)
}
func TestNativeMoneyPostgresRedeemTransactionRollback(t *testing.T) {
	f := realFixture(t)
	f.state.Store(2)
	_, err := f.db.Exec(`CREATE FUNCTION block_test_credit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.balance>OLD.balance THEN RAISE EXCEPTION 'injected balance commit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER block_test_credit BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION block_test_credit();`)
	require.NoError(t, err)
	require.NotEqual(t, "success", strings.TrimSpace(f.notify().Body.String()))
	require.Equal(t, "5.00000000", f.balance(t))
	var used int
	require.NoError(t, f.db.QueryRow("SELECT count(*) FROM redeem_codes WHERE status='used'").Scan(&used))
	require.Zero(t, used)
	_, err = f.db.Exec("DROP TRIGGER block_test_credit ON users")
	require.NoError(t, err)
	f.restart(t)
	_, err = f.svc.ReconcilePendingPaymentOrders(context.Background())
	require.NoError(t, err)
	f.assertSettled(t)
}
func TestNativeMoneyPostgresCrashAfterBalanceCommit(t *testing.T) {
	f := realFixture(t)
	f.state.Store(2)
	_, err := f.db.Exec(`CREATE FUNCTION block_test_complete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='COMPLETED' THEN RAISE EXCEPTION 'injected completion failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER block_test_complete BEFORE UPDATE ON payment_orders FOR EACH ROW EXECUTE FUNCTION block_test_complete();`)
	require.NoError(t, err)
	require.NotEqual(t, "success", strings.TrimSpace(f.notify().Body.String()))
	require.Equal(t, "105.00000000", f.balance(t))
	_, err = f.db.Exec("DROP TRIGGER block_test_complete ON payment_orders")
	require.NoError(t, err)
	f.restart(t)
	_, err = f.svc.ReconcilePendingPaymentOrders(context.Background())
	require.NoError(t, err)
	f.assertSettled(t)
}
