package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

const bepTestAddress = "0x1111111111111111111111111111111111111111"

var bepTestHash = "0x" + strings.Repeat("a", 64)

func bepTestProvider(t *testing.T, handler http.HandlerFunc) *BEPusdt {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, e := NewBEPusdt("1", map[string]string{"apiBase": srv.URL, "apiToken": "fixture-key", "notifyUrl": "https://main.example/api/v1/payment/webhook/bepusdt", "returnUrl": "https://main.example/payment/result"})
	require.NoError(t, e)
	return p
}
func bepTestData(tradeType string, state int) map[string]any {
	return map[string]any{"trade_id": "gateway_123", "order_id": "MAIN_USDT_order123", "trade_type": tradeType, "status": state, "fiat": "CNY", "amount": "200.00", "money": "200.00", "actual_amount": "29.850000000000000001", "token": bepTestAddress, "expiration_time": 599, "expired_at": time.Now().Add(590 * time.Second).Unix(), "payment_url": "/pay/checkout/gateway_123", "trade_url": "https://bscscan.com/tx/" + bepTestHash}
}
func bepRespond(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "data": data})
}

func TestBEPusdtNativeCreateAndQuery(t *testing.T) {
	for _, method := range []string{payment.TypeUSDTBSC, payment.TypeUSDTArbitrum} {
		t.Run(method, func(t *testing.T) {
			tradeType, network, _ := payment.BEPusdtTradeType(method)
			p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				var params map[string]any
				dec := json.NewDecoder(r.Body)
				dec.UseNumber()
				require.NoError(t, dec.Decode(&params))
				switch r.URL.Path {
				case "/api/v1/order/create-transaction":
					sig, e := bepSign(params, "fixture-key")
					require.NoError(t, e)
					require.Equal(t, sig, params["signature"])
					require.Equal(t, tradeType, params["trade_type"])
					require.Equal(t, "CNY", params["fiat"])
					require.Equal(t, json.Number("200.00"), params["amount"])
					require.NotContains(t, params, "rate")
					bepRespond(w, bepTestData(tradeType, 1))
				case "/api/v1/pay/info":
					require.Equal(t, "gateway_123", params["trade_id"])
					bepRespond(w, bepTestData(tradeType, 5))
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			result, e := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: method, Amount: "200.00", ExpiresAt: time.Now().Add(600 * time.Second)})
			require.NoError(t, e)
			require.Equal(t, network, result.CryptoPayment.Network)
			require.Equal(t, "29.850000000000000001", result.CryptoPayment.ChainAmount)
			require.Equal(t, "CNY", result.Currency)
			query, e := p.QueryOrder(context.Background(), result.TradeNo)
			require.NoError(t, e)
			require.Equal(t, payment.ProviderStatusConfirming, query.Status)
			require.Equal(t, 200.0, query.Amount)
		})
	}
}
func TestBEPusdtCreationUnknownAndResponseBinding(t *testing.T) {
	for _, field := range []string{"order_id", "trade_type", "fiat", "amount", "actual_amount", "token", "payment_url"} {
		t.Run(field, func(t *testing.T) {
			p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				data := bepTestData("usdt.bep20", 1)
				data[field] = "invalid"
				if field == "amount" {
					delete(data, "money")
				}
				bepRespond(w, data)
			})
			_, e := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: time.Now().Add(600 * time.Second)})
			require.ErrorIs(t, e, payment.ErrPaymentOutcomeUnknown)
		})
	}
	p := bepTestProvider(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_, e := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: time.Now().Add(time.Minute * 10)})
	require.ErrorIs(t, e, payment.ErrPaymentOutcomeUnknown)
}
func TestBEPusdtSignedCallbackAuthenticatesClaimsWithoutRemoteQuery(t *testing.T) {
	p := bepTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("signature parsing must not make a remote query")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	payload := map[string]any{"trade_id": "gateway_123", "order_id": "MAIN_USDT_order123", "amount": json.Number("200"), "actual_amount": "29.850000000000000001", "token": bepTestAddress, "block_transaction_id": bepTestHash, "status": 2}
	sign := func(data map[string]any) string {
		signature, e := bepSign(data, "fixture-key")
		require.NoError(t, e)
		data["signature"] = signature
		b, e := json.Marshal(data)
		require.NoError(t, e)
		return string(b)
	}
	valid := sign(payload)
	n, e := p.VerifyNotification(context.Background(), valid, nil)
	require.NoError(t, e)
	require.Equal(t, "MAIN_USDT_order123", n.OrderID)
	require.Equal(t, 200.0, n.Amount)
	for _, field := range []string{"order_id", "amount", "actual_amount", "token", "block_transaction_id"} {
		t.Run(field, func(t *testing.T) {
			data := map[string]any{}
			for k, v := range payload {
				data[k] = v
			}
			data[field] = "invalid"
			_, e := p.VerifyNotification(context.Background(), sign(data), nil)
			require.Error(t, e)
		})
	}
	_, e = p.VerifyNotification(context.Background(), strings.Replace(valid, "MAIN_USDT_order123", "DJP_order123", 1), nil)
	require.Error(t, e)
	_, e = p.VerifyNotification(context.Background(), valid+"{}", nil)
	require.Error(t, e)
}
func TestBEPusdtStateAndUnsupportedOperations(t *testing.T) {
	expected := []string{"pending", "paid", "expired", "cancelled", "confirming", "failed"}
	for i, s := range expected {
		actual, e := bepState(i + 1)
		require.NoError(t, e)
		require.Equal(t, s, actual)
	}
	_, e := bepState(0)
	require.Error(t, e)
	p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/order/cancel-transaction", r.URL.Path)
		bepRespond(w, map[string]string{"trade_id": "gateway_123"})
	})
	require.NoError(t, p.CancelPayment(context.Background(), "gateway_123"))
	_, e = p.Refund(context.Background(), payment.RefundRequest{})
	require.ErrorIs(t, e, payment.ErrRefundUnsupported)
	for _, amount := range []string{"0", "-1", "1e3", "NaN", "0.0000000000000000001"} {
		require.False(t, bepChainAmount(amount), fmt.Sprintf("amount %s", amount))
	}
}

func TestBEPusdtAllNetworksAndPinnedAddress(t *testing.T) {
	for _, network := range payment.BEPusdtNetworks() {
		t.Run(network.Method, func(t *testing.T) {
			addr := bepTestAddress
			hash := bepTestHash
			txURL := "https://scan.example/tx/" + hash
			switch network.AddressKind {
			case "tron":
				addr = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
				hash = strings.Repeat("a", 64)
				txURL = "https://tronscan.org/#/transaction/" + hash
			case "solana":
				addr = strings.Repeat("1", 32)
				hash = strings.Repeat("1", 64)
				txURL = "https://solscan.io/tx/" + hash
			case "aptos":
				addr = "0x" + strings.Repeat("1", 64)
			}
			p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/order/create-transaction" {
					var req map[string]any
					_ = json.NewDecoder(r.Body).Decode(&req)
					require.Equal(t, addr, req["address"])
					require.Equal(t, network.TradeType, req["trade_type"])
				}
				data := bepTestData(network.TradeType, 2)
				if r.URL.Path == "/api/v1/order/create-transaction" {
					data["status"] = 1
				}
				data["token"] = addr
				data["trade_url"] = txURL
				bepRespond(w, data)
			})
			p.walletAddresses = map[string]string{network.Method: addr}
			created, e := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: network.Method, Amount: "200", ExpiresAt: time.Now().Add(10 * time.Minute)})
			require.NoError(t, e)
			require.Equal(t, addr, created.CryptoPayment.Address)
			q, e := p.QueryOrder(context.Background(), created.TradeNo)
			require.NoError(t, e)
			require.Equal(t, hash, q.CryptoPayment.TransactionHash)
			require.Equal(t, network.Network, q.CryptoPayment.Network)
			claims := map[string]any{"trade_id": "gateway_123", "order_id": "MAIN_USDT_order123", "amount": 200, "actual_amount": "29.850000000000000001", "token": addr, "block_transaction_id": hash, "status": 2}
			signature, e := bepSign(claims, "fixture-key")
			require.NoError(t, e)
			claims["signature"] = signature
			raw, _ := json.Marshal(claims)
			notification, e := p.VerifyNotification(context.Background(), string(raw), nil)
			require.NoError(t, e)
			require.Equal(t, addr, notification.Metadata["address"])
			require.Equal(t, hash, notification.Metadata["transaction_hash"])
		})
	}
}
func TestBEPusdtCreateRejectsUnexpectedReceivingAddress(t *testing.T) {
	p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) { bepRespond(w, bepTestData("usdt.bep20", 1)) })
	p.walletAddresses = map[string]string{payment.TypeUSDTBSC: "0x" + strings.Repeat("2", 40)}
	_, e := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: time.Now().Add(10 * time.Minute)})
	require.ErrorIs(t, e, payment.ErrPaymentOutcomeUnknown)
}

func TestBEPusdtResumeReturnURLFitsGatewayStorage(t *testing.T) {
	original := "https://main.example/payment/result?order_id=1457&out_trade_no=MAIN_USDT_order123&status=success&resume_token=" + strings.Repeat("signed-resume-credential", 20)
	require.Greater(t, len(original), 255)
	p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var params map[string]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		require.NoError(t, dec.Decode(&params))
		returned, ok := params["redirect_url"].(string)
		require.True(t, ok)
		if utf8.RuneCountInString(returned) > 255 {
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 400, "message": "value too long for type character varying(255)"})
			return
		}
		u, err := url.Parse(returned)
		require.NoError(t, err)
		require.Empty(t, u.Query().Get("resume_token"))
		require.Equal(t, "1457", u.Query().Get("order_id"))
		require.Equal(t, "MAIN_USDT_order123", u.Query().Get("out_trade_no"))
		sig, err := bepSign(params, "fixture-key")
		require.NoError(t, err)
		require.Equal(t, sig, params["signature"])
		bepRespond(w, bepTestData("usdt.bep20", 1))
	})
	result, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200.00", ReturnURL: original, ExpiresAt: time.Now().Add(600 * time.Second)})
	require.NoError(t, err)
	require.NotEmpty(t, result.PayURL)
	require.Equal(t, "pending", result.CryptoPayment.Status)
}

func TestBEPusdtStorageLimitsRejectBeforeRemoteCreation(t *testing.T) {
	p := bepTestProvider(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid fields must not reach the gateway") })
	for _, req := range []payment.CreatePaymentRequest{
		{Subject: strings.Repeat("名", 65)},
		{ReturnURL: "https://main.example/payment/result?keep=" + strings.Repeat("x", 260)},
		{ReturnURL: "https://main.example/payment/result?x=%xx"},
	} {
		req.OrderID = "MAIN_USDT_order123"
		req.Amount = "200"
		req.PaymentType = payment.TypeUSDTBSC
		req.ExpiresAt = time.Now().Add(600 * time.Second)
		_, err := p.CreatePayment(context.Background(), req)
		require.Error(t, err)
		require.NotErrorIs(t, err, payment.ErrPaymentOutcomeUnknown)
	}
	for _, key := range []string{"notifyUrl", "returnUrl"} {
		cfg := map[string]string{"apiBase": "https://pay.example", "apiToken": "fixture-secret", "notifyUrl": "https://main.example/hook", "returnUrl": "https://main.example/result"}
		cfg[key] = "https://main.example/" + strings.Repeat("x", 256)
		_, err := NewBEPusdt("1", cfg)
		require.Error(t, err)
	}
}

func TestBEPusdtRejectionDiagnosticDoesNotLeakGatewayMessage(t *testing.T) {
	p := bepTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 400, "message": "echoed-private-resume-token"})
	})
	_, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", Amount: "200", PaymentType: payment.TypeUSDTBSC, ExpiresAt: time.Now().Add(600 * time.Second)})
	require.ErrorIs(t, err, payment.ErrPaymentOutcomeUnknown)
	require.Contains(t, err.Error(), "status_code=400")
	require.NotContains(t, err.Error(), "echoed-private-resume-token")
}

func TestBEPusdtConfiguredDeadlineAndGatewayDefaultMismatch(t *testing.T) {
	for _, minutes := range []int{15, 30, 60} {
		t.Run(fmt.Sprint(minutes), func(t *testing.T) {
			p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				var params map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&params))
				timeout, ok := params["timeout"].(float64)
				require.True(t, ok)
				ttl := int64(timeout)
				require.InDelta(t, minutes*60, ttl, 2)
				data := bepTestData("usdt.bep20", 1)
				data["expiration_time"] = ttl
				bepRespond(w, data)
			})
			deadline := time.Now().Add(time.Duration(minutes) * time.Minute)
			result, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: deadline})
			require.NoError(t, err)
			require.WithinDuration(t, deadline, result.CryptoPayment.ExpiresAt, 2*time.Second)
		})
	}
	for _, seconds := range []int{179, 3602} {
		p := bepTestProvider(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid deadline must not reach gateway") })
		_, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: time.Now().Add(time.Duration(seconds) * time.Second)})
		require.Error(t, err)
		require.NotErrorIs(t, err, payment.ErrPaymentOutcomeUnknown)
	}
	for _, returned := range []int{600, 3600} {
		p := bepTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			data := bepTestData("usdt.bep20", 1)
			data["expiration_time"] = returned
			bepRespond(w, data)
		})
		_, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200", ExpiresAt: time.Now().Add(30 * time.Minute)})
		require.ErrorIs(t, err, payment.ErrPaymentOutcomeUnknown)
	}
}

func TestBEPusdtReturnedDeadlineAccountsForGatewayProcessingTime(t *testing.T) {
	start := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	deadline := start.Add(30 * time.Minute)
	require.True(t, bepDeadlineMatches(deadline, 1792, start.Add(8*time.Second)))
	require.False(t, bepDeadlineMatches(deadline, 600, start.Add(8*time.Second)))
	require.False(t, bepDeadlineMatches(deadline, 1800, start.Add(8*time.Second)))
	require.False(t, bepDeadlineMatches(deadline, 0, start.Add(8*time.Second)))
}
