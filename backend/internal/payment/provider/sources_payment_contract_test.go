package provider

// Audit overlay: copied into immutable source exports, never the running app.
// Every upstream below is a loopback httptest server with synthetic credentials.
import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestSourcesPaymentContractQuery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reject     bool
	}{
		{"genuine_paid", `{"code":1,"status":1,"money":"100.00","trade_no":"audit-trade","out_trade_no":"audit-order","pid":"pid-1","type":"alipay"}`, false},
		{"failed_envelope_with_paid_fields", `{"code":0,"status":1,"money":"100.00","trade_no":"audit-trade","out_trade_no":"audit-order","pid":"pid-1","type":"alipay"}`, true},
		{"paid_without_trade_identity", `{"code":1,"status":1,"money":"100.00"}`, true},
		{"query_error_is_unknown", `{"code":0,"msg":"fixture query unavailable"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := newTestEasyPay(t, srv.URL)
			got, err := p.QueryOrder(context.Background(), "audit-order")
			if tc.reject && err == nil {
				t.Fatalf("untrusted query accepted: status=%s trade=%s", got.Status, got.TradeNo)
			}
			if !tc.reject && (err != nil || got == nil || got.Status != payment.ProviderStatusPaid) {
				t.Fatalf("genuine paid query rejected: got=%v err=%v", got, err)
			}
		})
	}
}

func TestSourcesPaymentContractNotification(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		duplicate, missingTrade bool
	}{
		{"genuine", false, false}, {"duplicate_money", true, false}, {"missing_trade", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestEasyPay(t, "http://127.0.0.1:1")
			params := map[string]string{"pid": "pid-1", "out_trade_no": "audit-order", "trade_no": "audit-trade", "type": "alipay", "money": "100.00", "trade_status": "TRADE_SUCCESS"}
			if tc.missingTrade {
				delete(params, "trade_no")
			}
			params["sign"] = easyPaySign(params, "pkey-1")
			v := url.Values{}
			for k, value := range params {
				v.Set(k, value)
			}
			if tc.duplicate {
				v.Add("money", "0.01")
			}
			_, err := p.VerifyNotification(context.Background(), v.Encode(), nil)
			if (tc.duplicate || tc.missingTrade) && err == nil {
				t.Fatal("ambiguous or identity-free notification accepted")
			}
			if !tc.duplicate && !tc.missingTrade && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourcesPaymentContractQueryNeverFollowsRedirect(t *testing.T) {
	var forwarded atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); _, _ = w.Write([]byte(`{"code":0}`)) }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	p := newTestEasyPay(t, first.URL)
	_, _ = p.QueryOrder(context.Background(), "audit-order")
	if forwarded.Load() != 0 {
		t.Fatalf("query credentials forwarded to a second origin: requests=%d", forwarded.Load())
	}
}
