package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWalletBridgeSendsOnlyMetadataAndScopedCapability(t *testing.T) {
	svc := newTestService(t)
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Refresh(context.Background(), src.ID); err != nil {
		t.Fatal(err)
	}
	var received atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/internal/sources-wallet/snapshot" || r.Header.Get("Authorization") != "Bearer fixture-capability" {
			t.Error("wrong bridge target or authentication")
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "encrypted_token") || strings.Contains(string(raw), "personal-fixture") {
			t.Error("credential crossed bridge")
		}
		var payload struct {
			GeneratedAt string `json:"generated_at"`
			Sources     []struct {
				ID          string `json:"id"`
				LastBalance *struct {
					Remaining string `json:"remaining"`
				} `json:"last_balance"`
			} `json:"sources"`
		}
		if json.Unmarshal(raw, &payload) != nil || len(payload.Sources) != 1 || payload.Sources[0].ID != src.ID || payload.Sources[0].LastBalance.Remaining != "2.5" {
			t.Error("invalid snapshot")
		}
		received.Store(true)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{"synchronized":true}}`)
	}))
	defer server.Close()
	bridge := NewWalletBridge(server.URL, "fixture-capability")
	if err = bridge.Push(context.Background(), svc.Store); err != nil || !received.Load() {
		t.Fatalf("bridge failed: %v", err)
	}
	if bridge.lastSuccess.Load() == 0 {
		t.Fatal("missing synchronization receipt")
	}
}

func TestWalletBridgeRequiresNativeSynchronizationReceipt(t *testing.T) {
	const success = `{"code":0,"message":"success","data":{"synchronized":true}}`
	tests := []struct {
		name, contentType, body string
		status                  int
		want                    error
	}{
		{"native_success", "application/json; charset=utf-8", success + "\n", 200, nil},
		{"spa_html", "text/html; charset=utf-8", "<!doctype html><html>SPA</html>", 200, ErrResponse},
		{"html_with_json_header", "application/json", "<html>SPA</html>", 200, ErrResponse},
		{"empty", "application/json", "", 200, ErrResponse},
		{"malformed", "application/json", `{"code":0,`, 200, ErrResponse},
		{"null", "application/json", "null", 200, ErrResponse},
		{"missing_code", "application/json", `{"data":{"synchronized":true}}`, 200, ErrResponse},
		{"null_code", "application/json", `{"code":null,"data":{"synchronized":true}}`, 200, ErrResponse},
		{"failed_code", "application/json", `{"code":500,"data":{"synchronized":true}}`, 200, ErrResponse},
		{"string_code", "application/json", `{"code":"0","data":{"synchronized":true}}`, 200, ErrResponse},
		{"missing_data", "application/json", `{"code":0}`, 200, ErrResponse},
		{"null_data", "application/json", `{"code":0,"data":null}`, 200, ErrResponse},
		{"missing_confirmation", "application/json", `{"code":0,"data":{}}`, 200, ErrResponse},
		{"false_confirmation", "application/json", `{"code":0,"data":{"synchronized":false}}`, 200, ErrResponse},
		{"string_confirmation", "application/json", `{"code":0,"data":{"synchronized":"true"}}`, 200, ErrResponse},
		{"trailing_document", "application/json", success + `{}`, 200, ErrResponse},
		{"oversized_valid_json", "application/json", success + strings.Repeat(" ", walletBridgeResponseLimit), 200, ErrResponse},
		{"wrong_content_type", "text/plain", success, 200, ErrResponse},
		{"missing_content_type", "", success, 200, ErrResponse},
		{"unsuccessful_http_status", "application/json", success, 503, ErrTransport},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newTestService(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "application/json" {
					t.Error("missing native JSON response negotiation")
				}
				w.Header()["Content-Type"] = []string{test.contentType}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			bridge := NewWalletBridge(server.URL, "fixture-capability")
			bridge.lastSuccess.Store(123)
			if err := bridge.Push(context.Background(), svc.Store); !errors.Is(err, test.want) {
				t.Fatalf("Push() = %v, want %v", err, test.want)
			}
			if test.want != nil && bridge.lastSuccess.Load() != 123 {
				t.Fatal("invalid acknowledgment advanced the last successful synchronization")
			}
			if test.want == nil && bridge.lastSuccess.Load() <= 123 {
				t.Fatal("valid acknowledgment did not update synchronization time")
			}
		})
	}
}

type bridgeResponseTransport func(*http.Request) (*http.Response, error)

func (f bridgeResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type bridgeCountingBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *bridgeCountingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *bridgeCountingBody) Close() error { b.closed = true; return nil }

func TestWalletBridgeBoundsAndClosesResponseBody(t *testing.T) {
	svc := newTestService(t)
	body := &bridgeCountingBody{Reader: strings.NewReader(strings.Repeat("x", 100*walletBridgeResponseLimit))}
	bridge := NewWalletBridge("http://bridge.validation.invalid", "fixture-capability")
	bridge.Client.Transport = bridgeResponseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
	})
	if err := bridge.Push(context.Background(), svc.Store); !errors.Is(err, ErrResponse) {
		t.Fatalf("oversized response: %v", err)
	}
	if body.read > walletBridgeResponseLimit+1 || !body.closed || bridge.lastSuccess.Load() != 0 {
		t.Fatalf("unbounded/unclosed/accepted response: read=%d closed=%t", body.read, body.closed)
	}
}

func TestWalletBridgeRejectsTruncatedNativeReceipt(t *testing.T) {
	svc := newTestService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "200")
		_, _ = io.WriteString(w, `{"code":0,"data":{"synchronized":true}}`)
	}))
	defer server.Close()
	bridge := NewWalletBridge(server.URL, "fixture-capability")
	if err := bridge.Push(context.Background(), svc.Store); !errors.Is(err, ErrTransport) || bridge.lastSuccess.Load() != 0 {
		t.Fatalf("truncated transport must not count as success: %v", err)
	}
}

func TestWalletBridgeDoesNotFollowRedirectOrForwardCapability(t *testing.T) {
	svc := newTestService(t)
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"synchronized":true}}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	bridge := NewWalletBridge(origin.URL, "fixture-capability")
	if err := bridge.Push(context.Background(), svc.Store); !errors.Is(err, ErrTransport) || redirected.Load() != 0 || bridge.lastSuccess.Load() != 0 {
		t.Fatalf("redirect followed or recorded as success: %v", err)
	}
}

func TestWalletBridgeHonorsCallerDeadlineWhileReadingReceipt(t *testing.T) {
	svc := newTestService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	bridge := NewWalletBridge(server.URL, "fixture-capability")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := bridge.Push(ctx, svc.Store); !errors.Is(err, ErrTransport) || bridge.lastSuccess.Load() != 0 {
		t.Fatalf("cancelled receipt recorded as success: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("caller deadline did not bound response reading")
	}
}
