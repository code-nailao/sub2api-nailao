//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

// This starts at the verified notification seam. The gateway says UNPAID.
// A verified notification must prompt a query, not itself authorize a credit.
// The balance repository records actual calls made by the real fulfillment path.
func TestSourcesPaymentContractUnpaidQueryNeverCredits(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	var queries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"status":0,"money":"100.00","trade_no":"audit-trade","out_trade_no":"audit-order","pid":"audit-merchant","type":"alipay"}`))
	}))
	defer srv.Close()
	cfg, err := json.Marshal(map[string]string{"apiBase": srv.URL, "pid": "audit-merchant", "pkey": "audit-only-secret", "notifyUrl": "https://main.example/api/v1/payment/webhook/easypay", "returnUrl": "https://main.example/payment/result"})
	require.NoError(t, err)
	inst, err := client.PaymentProviderInstance.Create().SetProviderKey(payment.TypeEasyPay).SetName("isolated audit").SetConfig(string(cfg)).SetSupportedTypes(payment.TypeAlipay).SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("audit@example.test").SetUsername("audit").SetPasswordHash("test-only").Save(ctx)
	require.NoError(t, err)
	order, err := client.PaymentOrder.Create().SetUserID(user.ID).SetUserEmail(user.Email).SetUserName(user.Username).SetAmount(100).SetPayAmount(100).SetRechargeCode("AUDIT-ONLY").SetOutTradeNo("audit-order").SetPaymentTradeNo("audit-trade").SetPaymentType(payment.TypeAlipay).SetProviderKey(payment.TypeEasyPay).SetProviderInstanceID(strconv.FormatInt(inst.ID, 10)).SetOrderType(payment.OrderTypeBalance).SetStatus(OrderStatusPending).SetExpiresAt(time.Now().Add(time.Hour)).SetClientIP("127.0.0.1").SetSrcHost("main.example").Save(ctx)
	require.NoError(t, err)
	credited := 0.0
	users := &mockUserRepo{getByIDUser: &User{ID: user.ID}}
	users.updateBalanceFn = func(_ context.Context, _ int64, amount float64) error { credited += amount; return nil }
	codes := &paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{order.RechargeCode: {ID: 1, Code: order.RechargeCode, Type: RedeemTypeBalance, Value: 100, Status: StatusUnused}}}
	svc := &PaymentService{entClient: client, registry: payment.NewRegistry(), loadBalancer: payment.NewDefaultLoadBalancer(client, nil), configService: NewPaymentConfigService(client, nil, nil), userRepo: users, redeemService: NewRedeemService(codes, users, nil, nil, nil, client, nil, nil)}
	err = svc.HandlePaymentNotification(ctx, &payment.PaymentNotification{OrderID: order.OutTradeNo, TradeNo: "audit-trade", Amount: 100, Status: payment.NotificationStatusSuccess, Metadata: map[string]string{"pid": "audit-merchant"}}, payment.TypeEasyPay)
	got, readErr := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, readErr)
	t.Logf("gateway_unpaid queries=%d credited=%.2f status=%s notification_error=%v", queries.Load(), credited, got.Status, err)
	if credited != 0 || got.Status != OrderStatusPending || queries.Load() != 1 || err == nil {
		t.Fatalf("unpaid confirmation gate violated: queries=%d credited=%.2f status=%s error=%v", queries.Load(), credited, got.Status, err)
	}
}
