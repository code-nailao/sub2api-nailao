//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/proof"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	"github.com/stretchr/testify/require"
)

type verifiedFixture struct {
	svc       *PaymentService
	order     *dbent.PaymentOrder
	selection *payment.InstanceSelection
	state     atomic.Int32
	calls     atomic.Int32
	credits   atomic.Int64
}

func newVerifiedFixture(t *testing.T, method string) *verifiedFixture {
	t.Helper()
	return newVerifiedFixtureWithClient(t, method, newPaymentOrderLifecycleTestClient(t))
}

func newVerifiedFixtureWithClient(t *testing.T, method string, client *dbent.Client) *verifiedFixture {
	t.Helper()
	ctx := context.Background()
	installPaymentProofTestSchema(t, client)
	f := &verifiedFixture{}
	f.state.Store(1)
	n, ok := payment.BEPusdtNetworkByMethod(method)
	require.True(t, ok)
	addr := "0x1111111111111111111111111111111111111111"
	hash := "0x" + strings.Repeat("a", 64)
	txURL := "https://scan.example/tx/" + hash
	if n.Network == "tron" {
		addr = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
		hash = strings.Repeat("a", 64)
		txURL = "https://tronscan.org/#/transaction/" + hash
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path == "/api/v1/pay/info" {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["trade_id"] != "native_test" {
				http.Error(w, "unknown gateway trade", 404)
				return
			}
		}
		if f.state.Load() == 99 {
			http.Error(w, "unavailable", 503)
			return
		}
		if r.URL.Path == "/api/v1/order/cancel-transaction" {
			f.state.Store(4)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "data": map[string]any{"trade_id": "native_test", "order_id": "MAIN_USDT_verified", "trade_type": n.TradeType, "fiat": "CNY", "money": "100.00", "amount": "100.00", "actual_amount": "14.250000000000000001", "token": addr, "status": f.state.Load(), "expired_at": time.Now().Add(9 * time.Minute).Unix(), "expiration_time": 599, "payment_url": "/pay/checkout/native_test", "trade_url": txURL}})
	}))
	t.Cleanup(server.Close)
	cfg := map[string]string{"apiBase": server.URL, "apiToken": "test-only", "notifyUrl": "https://main.example/api/v1/payment/webhook/bepusdt", "returnUrl": "https://main.example/payment/result"}
	raw, _ := json.Marshal(cfg)
	i, err := client.PaymentProviderInstance.Create().SetProviderKey(payment.TypeBEPusdt).SetName("native test").SetConfig(string(raw)).SetSupportedTypes(method).SetEnabled(true).SetPaymentMode("redirect").Save(ctx)
	require.NoError(t, err)
	u, err := client.User.Create().SetEmail("native@example.test").SetUsername("native").SetPasswordHash("test-only").Save(ctx)
	require.NoError(t, err)
	o, err := client.PaymentOrder.Create().SetUserID(u.ID).SetUserEmail(u.Email).SetUserName(u.Username).SetAmount(110).SetPayAmount(100).SetRechargeCode("NATIVE-TEST").SetOutTradeNo("MAIN_USDT_verified").SetPaymentType(method).SetPaymentTradeNo("native_test").SetProviderKey(payment.TypeBEPusdt).SetProviderInstanceID(strconv.FormatInt(i.ID, 10)).SetOrderType(payment.OrderTypeBalance).SetStatus(OrderStatusPending).SetExpiresAt(time.Now().Add(10 * time.Minute)).SetClientIP("127.0.0.1").SetSrcHost("main.example").Save(ctx)
	require.NoError(t, err)
	users := &mockUserRepo{getByIDUser: &User{ID: u.ID, Email: u.Email, Status: StatusActive}}
	users.updateBalanceFn = func(_ context.Context, _ int64, amount float64) error { f.credits.Add(int64(amount)); return nil }
	codes := &paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{o.RechargeCode: {ID: 1, Code: o.RechargeCode, Type: RedeemTypeBalance, Value: 110, Status: StatusUnused}}}
	f.svc = &PaymentService{entClient: client, registry: payment.NewRegistry(), loadBalancer: payment.NewDefaultLoadBalancer(client, nil), configService: NewPaymentConfigService(client, nil, nil), userRepo: users, redeemService: NewRedeemService(codes, users, nil, nil, nil, client, nil, nil)}
	f.order = o
	f.selection = &payment.InstanceSelection{InstanceID: strconv.FormatInt(i.ID, 10), ProviderKey: payment.TypeBEPusdt, Config: cfg, PaymentMode: "redirect", SupportedTypes: method}
	return f
}

func (f *verifiedFixture) notify() error {
	return f.svc.HandlePaymentNotification(context.Background(), &payment.PaymentNotification{OrderID: f.order.OutTradeNo, TradeNo: "native_test", Amount: 100, Status: payment.NotificationStatusSuccess}, payment.TypeBEPusdt)
}

func TestVerifiedNativePaymentLifecycle(t *testing.T) {
	for _, method := range []string{"usdt_bsc", "usdt_arbitrum", "usdt_tron", "usdt_xlayer"} {
		t.Run(method, func(t *testing.T) {
			f := newVerifiedFixture(t, method)
			require.Error(t, f.notify())
			require.Zero(t, f.credits.Load())
			f.state.Store(2)
			require.NoError(t, f.notify())
			require.NoError(t, f.notify())
			require.Equal(t, int64(110), f.credits.Load())
			r, err := proof.Load(context.Background(), f.svc.entClient, f.order.ID)
			require.NoError(t, err)
			require.Equal(t, "100", r.Amount)
			require.NotEmpty(t, r.ChainIdentity)
		})
	}
}

func TestVerifiedNativeUnknownAndCancel(t *testing.T) {
	for _, state := range []int32{5, 99} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			f := newVerifiedFixture(t, "usdt_bsc")
			f.state.Store(state)
			result, err := f.svc.CancelOrder(context.Background(), f.order.ID, f.order.UserID)
			require.Error(t, err)
			require.Equal(t, paymentVerificationPending, result)
			require.Zero(t, f.credits.Load())
			o, err := f.svc.entClient.PaymentOrder.Get(context.Background(), f.order.ID)
			require.NoError(t, err)
			require.Equal(t, OrderStatusPending, o.Status)
		})
	}
}

func TestVerifiedNativeReceiptFailureRollsBack(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	f.state.Store(2)
	ctx := context.Background()
	_, err := f.svc.entClient.ExecContext(ctx, `CREATE TRIGGER reject_payment_audit BEFORE INSERT ON payment_audit_logs WHEN NEW.action='ORDER_PAID' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END;`)
	require.NoError(t, err)
	require.Error(t, f.notify())
	require.Zero(t, f.credits.Load())
	_, err = proof.Load(ctx, f.svc.entClient, f.order.ID)
	require.Error(t, err)
	o, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, o.Status)
	_, err = f.svc.entClient.ExecContext(ctx, `DROP TRIGGER reject_payment_audit`)
	require.NoError(t, err)
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
}

func TestVerifiedNativeVersionSurvivesDisabledInstance(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	ctx := context.Background()
	snapshot := buildPaymentOrderProviderSnapshot(f.selection, CreateOrderRequest{})
	require.NoError(t, f.svc.pinVerifiedProvider(ctx, f.svc.entClient, f.selection, snapshot))
	_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetProviderSnapshot(snapshot).Save(ctx)
	require.NoError(t, err)
	id, _ := strconv.ParseInt(f.selection.InstanceID, 10, 64)
	_, err = f.svc.entClient.PaymentProviderInstance.UpdateOneID(id).SetEnabled(false).SetConfig(`{"apiBase":"https://wrong.example","apiToken":"rotated"}`).Save(ctx)
	require.NoError(t, err)
	f.state.Store(2)
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
}

func TestVerifiedNativeUnpaidFailedCannotCredit(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetStatus(OrderStatusFailed).Save(context.Background())
	require.NoError(t, err)
	require.Error(t, f.svc.ExecuteBalanceFulfillment(context.Background(), f.order.ID))
	require.Zero(t, f.credits.Load())
}

func TestVerifiedNativeCheckoutUsesOnlyGatewayUI(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	f.selection.Config["checkoutPresentation"] = "main"
	f.selection.Config["checkoutCopy"] = "retired"
	resp, err := f.svc.invokeProvider(context.Background(), f.order, CreateOrderRequest{UserID: f.order.UserID, PaymentType: f.order.PaymentType, OrderType: payment.OrderTypeBalance, Amount: 100, ReturnURL: "https://untrusted.example/payment/result"}, &PaymentConfig{}, 100, "100.00", 100, nil, f.selection)
	require.NoError(t, err)
	require.Contains(t, resp.PayURL, "/pay/checkout/native_test")
	require.Equal(t, "redirect", resp.PaymentMode)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	for _, bad := range []string{"/main/checkout", "crypto_checkout_url", "test-only", "untrusted.example"} {
		require.NotContains(t, string(raw), bad)
	}
}

func TestVerifiedNativeLateUnknownCreationReview(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	ctx := context.Background()
	var err error
	f.order, err = f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).
		SetPaymentTradeNo("").SetExpiresAt(time.Now().Add(-24 * time.Hour)).Save(ctx)
	require.NoError(t, err)
	f.state.Store(2)
	require.NoError(t, f.notify())
	o, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPaymentReview, o.Status)
	require.Equal(t, "native_test", o.PaymentTradeNo)
	require.Zero(t, f.credits.Load())
	require.NoError(t, f.svc.RetryFulfillment(ctx, o.ID))
	require.Equal(t, int64(110), f.credits.Load())
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
}

func TestVerifiedNativeHistoricalPaidWithoutReceipt(t *testing.T) {
	for _, status := range []string{OrderStatusPaid, OrderStatusFailed, OrderStatusRecharging} {
		t.Run(status, func(t *testing.T) {
			f := newVerifiedFixture(t, "usdt_bsc")
			ctx := context.Background()
			_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetStatus(status).
				SetPaidAt(time.Now().Add(-time.Hour)).SetUpdatedAt(time.Now().Add(-time.Hour)).Save(ctx)
			require.NoError(t, err)
			// A historical PAID field is never sufficient evidence.
			require.NoError(t, f.svc.reconcileVerifiedPayments(ctx))
			require.Zero(t, f.credits.Load())
			f.state.Store(2)
			_, err = f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetUpdatedAt(time.Now().Add(-time.Hour)).Save(ctx)
			require.NoError(t, err)
			require.NoError(t, f.svc.reconcileVerifiedPayments(ctx))
			require.Equal(t, int64(110), f.credits.Load())
			require.NoError(t, f.svc.reconcileVerifiedPayments(ctx))
			require.Equal(t, int64(110), f.credits.Load())
		})
	}
}

func TestVerifiedNativeStoreCreation(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	p, err := provider.NewBEPusdt("test", f.selection.Config)
	require.NoError(t, err)
	out, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: f.order.OutTradeNo, Amount: "100.00", PaymentType: f.order.PaymentType, ExpiresAt: f.order.ExpiresAt, ReturnURL: f.selection.Config["returnUrl"]})
	require.NoError(t, err)
	require.NoError(t, f.svc.storeNativePaymentCreation(context.Background(), f.order, out))
}

func TestVerifiedNativeReconcileUnknownCreation(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	ctx := context.Background()
	_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetPaymentTradeNo("").Save(ctx)
	require.NoError(t, err)
	require.Error(t, f.svc.ReconcileNativePayment(ctx, f.order.ID, "wrong_reference"))
	require.Zero(t, f.credits.Load())
	require.NoError(t, f.svc.ReconcileNativePayment(ctx, f.order.ID, "native_test"))
	require.Zero(t, f.credits.Load())
	row, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, "native_test", row.PaymentTradeNo)
	f.state.Store(2)
	require.NoError(t, f.svc.ReconcileNativePayment(ctx, f.order.ID, "native_test"))
	require.Equal(t, int64(110), f.credits.Load())
	require.NoError(t, f.notify())
	require.Equal(t, int64(110), f.credits.Load())
}

func TestVerifiedLegacyEasyPayKeepsOriginalDisabledInstance(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	ctx := context.Background()
	alias := fmt.Sprintf("sub2_%d", f.order.ID)
	var oldQueries atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldQueries.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("out_trade_no") != alias {
			http.Error(w, "wrong original order", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "status": 1, "money": "100.00", "out_trade_no": alias, "trade_no": "old_trade", "type": "alipay", "pid": "merchant-old"})
	}))
	defer old.Close()
	cfg := map[string]string{"apiBase": old.URL, "pid": "merchant-old", "pkey": "old-test-secret", "notifyUrl": "https://main.example/api/v1/payment/webhook/easypay", "returnUrl": "https://main.example/payment/result"}
	raw, _ := json.Marshal(cfg)
	iid, err := strconv.ParseInt(f.selection.InstanceID, 10, 64)
	require.NoError(t, err)
	_, err = f.svc.entClient.PaymentProviderInstance.UpdateOneID(iid).SetProviderKey(payment.TypeEasyPay).SetSupportedTypes("alipay").SetConfig(string(raw)).SetEnabled(false).Save(ctx)
	require.NoError(t, err)
	cfg["pid"] = "merchant-new"
	cfg["apiBase"] = "https://uncontacted.invalid"
	cfg["pkey"] = "new-test-secret"
	raw, _ = json.Marshal(cfg)
	_, err = f.svc.entClient.PaymentProviderInstance.Create().SetProviderKey(payment.TypeEasyPay).SetName("new instance").SetConfig(string(raw)).SetSupportedTypes("alipay").SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	_, err = f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetProviderKey(payment.TypeEasyPay).SetPaymentType("alipay").SetOutTradeNo("").SetPaymentTradeNo("").Save(ctx)
	require.NoError(t, err)
	ps, err := f.svc.GetWebhookProviders(ctx, payment.TypeEasyPay, alias)
	require.NoError(t, err)
	require.Len(t, ps, 1)
	q, err := ps[0].QueryOrder(ctx, alias)
	require.NoError(t, err)
	require.Equal(t, alias, q.OrderID)
	require.NoError(t, f.svc.HandlePaymentNotification(ctx, &payment.PaymentNotification{OrderID: alias, TradeNo: "old_trade", Amount: 100, Status: payment.NotificationStatusSuccess}, payment.TypeEasyPay))
	require.Equal(t, int64(110), f.credits.Load())
	require.Equal(t, int32(2), oldQueries.Load())
	// A numeric alias must never address a modern random-reference order.
	_, err = f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetOutTradeNo("modern_random_ref").Save(ctx)
	require.NoError(t, err)
	_, err = f.svc.GetWebhookProviders(ctx, payment.TypeEasyPay, alias)
	require.ErrorIs(t, err, ErrOrderNotFound)
}

func TestVerifiedNativeExpiredUnknownEntersReviewWithoutPosting(t *testing.T) {
	f := newVerifiedFixture(t, "usdt_bsc")
	ctx := context.Background()
	_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetPaymentTradeNo("").SetExpiresAt(time.Now().Add(-time.Minute)).Save(ctx)
	require.NoError(t, err)
	_, err = f.svc.reconcileVerifiedPaymentOrders(ctx)
	require.NoError(t, err)
	row, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPaymentReview, row.Status)
	require.Nil(t, row.PaidAt)
	require.Zero(t, f.calls.Load())
	require.Zero(t, f.credits.Load())
	require.True(t, f.svc.hasAuditLog(ctx, row.ID, "PAYMENT_CREATION_REVIEW"))
}
