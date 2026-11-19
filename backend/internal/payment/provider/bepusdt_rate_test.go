package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

type bepRateTransport func(*http.Request) (*http.Response, error)

func (f bepRateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func bepRateResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const bepValidRate = `{"code":"000000","success":true,"data":[{"pair":"USD_CNY","rate":0.14925},{"pair":"CNY_USD","rate":6.7000}]}`

func TestBEPusdtBinanceReferenceParsing(t *testing.T) {
	tests := []struct {
		name, body, want string
		status           int
	}{
		{"numeric exact decimal", bepValidRate, "6.7", 200},
		{"decimal precision", `{"code":"000000","success":true,"data":[{"pair":"CNY_USD","rate":"6.71234567"}]}`, "6.71234567", 200},
		{"missing", `{"code":"000000","success":true,"data":[]}`, "", 200},
		{"duplicate", `{"code":"000000","success":true,"data":[{"pair":"CNY_USD","rate":6.7},{"pair":"CNY_USD","rate":6.8}]}`, "", 200},
		{"failed", strings.Replace(bepValidRate, `"success":true`, `"success":false`, 1), "", 200},
		{"wrong code", strings.Replace(bepValidRate, "000000", "100001", 1), "", 200},
		{"redirect", bepValidRate, "", 302},
		{"throttled", bepValidRate, "", 429},
		{"HTML", "<html>blocked</html>", "", 200},
		{"oversized", strings.Repeat(" ", 256<<10) + bepValidRate, "", 200},
		{"trailing data", bepValidRate + `{}`, "", 200},
	}
	for _, val := range []string{"0", "-6.7", "0.14925", "20.1", "6.712345678", "null", `"NaN"`, `"6.7e0"`, `true`} {
		tests = append(tests, struct {
			name, body, want string
			status           int
		}{"invalid " + val, fmt.Sprintf(`{"code":"000000","success":true,"data":[{"pair":"CNY_USD","rate":%s}]}`, val), "", 200})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &BEPusdt{client: &http.Client{Transport: bepRateTransport(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, binanceFiatReferenceURL, r.URL.String())
				require.Equal(t, http.MethodGet, r.Method)
				require.Empty(t, r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("signature"))
				require.Nil(t, r.Body)
				deadline, ok := r.Context().Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
				return bepRateResponse(tt.status, tt.body), nil
			})}}
			rate, err := p.binanceReferenceRate(context.Background())
			if tt.want == "" {
				require.Error(t, err)
				require.Empty(t, rate)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, rate)
			}
		})
	}
}

func TestBEPusdtBinanceReferenceLockedAtCreation(t *testing.T) {
	for _, method := range []string{payment.TypeUSDTBSC, payment.TypeUSDTArbitrum} {
		t.Run(method, func(t *testing.T) {
			tradeType, _, _ := payment.BEPusdtTradeType(method)
			rateReads, creates, queries, cancels := 0, 0, 0, 0
			p := bepTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/order/create-transaction":
					params := decodeBepRateRequest(t, r)
					sig, err := bepSign(params, "fixture-key")
					require.NoError(t, err)
					require.Equal(t, sig, params["signature"])
					require.Equal(t, []string{"6.7", "6.8"}[creates], params["rate"])
					creates++
					bepRespond(w, bepTestData(tradeType, 1))
				case "/api/v1/pay/info":
					queries++
					bepRespond(w, bepTestData(tradeType, 1))
				case "/api/v1/order/cancel-transaction":
					cancels++
					bepRespond(w, map[string]string{"trade_id": "gateway_123"})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			transport := http.DefaultTransport
			p.rateSource = "binance_reference"
			p.client.Transport = bepRateTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != binanceFiatReferenceURL {
					return transport.RoundTrip(r)
				}
				rateReads++
				body := bepValidRate
				if rateReads > 1 {
					body = strings.Replace(body, "6.7000", "6.8", 1)
				}
				return bepRateResponse(200, body), nil
			})
			for i := 0; i < 2; i++ {
				result, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: method, Amount: "200.00", ExpiresAt: time.Now().Add(600 * time.Second)})
				require.NoError(t, err)
				require.Equal(t, "29.850000000000000001", result.CryptoPayment.ChainAmount)
			}
			_, err := p.QueryOrder(context.Background(), "gateway_123")
			require.NoError(t, err)
			require.NoError(t, p.CancelPayment(context.Background(), "gateway_123"))
			require.Equal(t, 2, rateReads)
			require.Equal(t, 2, creates)
			require.Equal(t, 1, queries)
			require.Equal(t, 1, cancels)
		})
	}
}

func TestBEPusdtBinanceReferenceFailureDoesNotCreate(t *testing.T) {
	for _, failure := range []string{"network", "cancelled", "schema", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			p := bepTestProvider(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call gateway") })
			p.rateSource = "binance_reference"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expiry := time.Now().Add(600 * time.Second)
			if failure == "deadline" {
				expiry = time.Now().Add(180*time.Second + 10*time.Millisecond)
			}
			p.client.Transport = bepRateTransport(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, binanceFiatReferenceURL, r.URL.String())
				switch failure {
				case "network":
					return nil, errors.New("offline")
				case "cancelled":
					cancel()
					return nil, ctx.Err()
				case "deadline":
					time.Sleep(30 * time.Millisecond)
					return bepRateResponse(200, bepValidRate), nil
				default:
					return bepRateResponse(200, `{}`), nil
				}
			})
			_, err := p.CreatePayment(ctx, payment.CreatePaymentRequest{OrderID: "MAIN_USDT_order123", PaymentType: payment.TypeUSDTBSC, Amount: "200.00", ExpiresAt: expiry})
			require.Error(t, err)
			require.NotErrorIs(t, err, payment.ErrPaymentOutcomeUnknown)
		})
	}
}

func TestBEPusdtRateSourceConfiguration(t *testing.T) {
	for _, source := range []string{"", "gateway", "binance_reference", "typo"} {
		p, err := NewBEPusdt("1", map[string]string{"apiBase": "https://pay.example", "apiToken": "secret", "notifyUrl": "https://main.example/hook", "returnUrl": "https://main.example/result", "rateSource": source})
		if source == "typo" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			expected := source
			if expected == "" {
				expected = payment.BEPusdtRateGateway
			}
			require.Equal(t, expected, p.rateSource)
		}
	}
}

func decodeBepRateRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var p map[string]any
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	require.NoError(t, d.Decode(&p))
	return p
}
