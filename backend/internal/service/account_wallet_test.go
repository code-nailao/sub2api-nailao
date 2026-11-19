package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestAccountWalletParserNeverUsesKeyOrSubscriptionRemaining(t *testing.T) {
	for _, tc := range []struct{ name, body, status, amount string }{
		{"zero", `{"mode":"unrestricted","isValid":true,"balance":0,"remaining":0,"unit":"USD"}`, "ready", "0"},
		{"negative", `{"mode":"unrestricted","isValid":true,"balance":-0.000000001,"remaining":-1e-9,"unit":"USD"}`, "ready", "-0.000000001"},
		{"precise", `{"mode":"unrestricted","isValid":true,"balance":12345678901234567890.1234,"unit":"USD"}`, "ready", "12345678901234567890.1234"},
		{"key", `{"mode":"quota_limited","isValid":true,"remaining":100,"balance":999,"unit":"USD"}`, "wallet_unavailable", ""},
		{"subscription", `{"mode":"unrestricted","isValid":true,"remaining":100,"subscription":{"daily_limit_usd":100},"unit":"USD"}`, "wallet_unavailable", ""},
		{"subscription-with-balance", `{"mode":"unrestricted","isValid":true,"balance":999,"subscription":{},"unit":"USD"}`, "wallet_unavailable", ""},
		{"remaining-only", `{"mode":"unrestricted","isValid":true,"remaining":100,"unit":"USD"}`, "wallet_unavailable", ""},
		{"mismatch", `{"mode":"unrestricted","isValid":true,"balance":10,"remaining":100,"unit":"USD"}`, "error", ""},
		{"missing-unit", `{"mode":"unrestricted","isValid":true,"balance":0}`, "error", ""},
		{"invalid", `{"mode":"unrestricted","isValid":false,"balance":100,"unit":"USD"}`, "error", ""},
		{"foreign", `{"success":true,"balance":100}`, "error", ""},
		{"huge-exponent", `{"mode":"unrestricted","isValid":true,"balance":1e999999999,"unit":"USD"}`, "error", ""},
		{"html", `<html>login</html>`, "error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := parseAccountWallet([]byte(tc.body), time.Now())
			require.Equal(t, tc.status, v.Status)
			require.Equal(t, tc.amount, v.Remaining)
		})
	}
}

type walletTransport struct {
	call func(*http.Request, string, int64) (*http.Response, error)
}

func (w walletTransport) Do(r *http.Request, p string, id int64, _ int) (*http.Response, error) {
	return w.call(r, p, id)
}
func (w walletTransport) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return w.Do(r, p, id, n)
}
func walletResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
}
func walletFixture(upstream HTTPUpstream) (*AccountWalletReader, *Account) {
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}
	return NewAccountWalletReader(&AccountTestService{cfg: cfg, httpUpstream: upstream}), &Account{ID: 1, Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "private-key", "base_url": "https://upstream.example/v1"}}
}
func TestAccountWalletUsesNativeCredentialProxyAndBoundedCache(t *testing.T) {
	var calls atomic.Int32
	upstream := walletTransport{call: func(r *http.Request, p string, id int64) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "https://upstream.example/v1/usage?days=1", r.URL.String())
		require.Equal(t, "Bearer private-key", r.Header.Get("Authorization"))
		require.Equal(t, int64(1), id)
		require.Contains(t, p, "127.0.0.1:1080")
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":10,"unit":"USD"}`), nil
	}}
	reader, a := walletFixture(upstream)
	id := int64(3)
	a.ProxyID = &id
	a.Proxy = &Proxy{ID: 3, Protocol: "socks5", Host: "127.0.0.1", Port: 1080}
	first := reader.Read(context.Background(), a, false)
	require.Equal(t, "ready", first.Status)
	require.Equal(t, first, reader.Read(context.Background(), a, true))
	require.Equal(t, int32(1), calls.Load())
	raw, _ := json.Marshal(first)
	require.NotContains(t, string(raw), "private-key")
	require.NotContains(t, string(raw), "upstream.example")
	identity := AccountWalletIdentity(a)
	a.Proxy.UpdatedAt = time.Now()
	require.Equal(t, identity, AccountWalletIdentity(a), "proxy bookkeeping must not invalidate the wallet cache")
	a.Credentials["api_key"] = "rotated"
	require.NotEqual(t, identity, AccountWalletIdentity(a))
	a.Credentials["api_key"] = "private-key"
	a.Proxy.Port = 1081
	require.NotEqual(t, identity, AccountWalletIdentity(a))
}
func TestAccountWalletFailureRetainsOnlySameIdentityHistory(t *testing.T) {
	var fail bool
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		if fail {
			return nil, errors.New("private-key upstream error")
		}
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":10,"unit":"USD"}`), nil
	}})
	initial := reader.Read(context.Background(), a, false)
	fail = true
	reader.mu.Lock()
	entry := reader.cache[AccountWalletIdentity(a)]
	entry.at = time.Now().Add(-time.Hour)
	reader.cache[AccountWalletIdentity(a)] = entry
	reader.mu.Unlock()
	failed := reader.Read(context.Background(), a, false)
	require.True(t, failed.Stale)
	require.Equal(t, initial.Remaining, failed.Remaining)
	require.Equal(t, "upstream_unavailable", failed.Error)
	a.Credentials["api_key"] = "changed"
	rotated := reader.Read(context.Background(), a, false)
	require.Empty(t, rotated.Remaining)
	require.Nil(t, rotated.CheckedAt)
}
func TestAccountWalletCoalescesConcurrentReads(t *testing.T) {
	var calls atomic.Int32
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(15 * time.Millisecond)
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":1,"unit":"USD"}`), nil
	}})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := reader.Read(context.Background(), a, false)
			if v.Status != "ready" {
				t.Errorf("unexpected status %s", v.Status)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
}
func TestAccountWalletSkipsMissingProxyOfficialHostsAndUnsupportedAccounts(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		t.Fatal("must not call upstream")
		return nil, nil
	}})
	id := int64(1)
	a.ProxyID = &id
	require.Equal(t, "proxy_unavailable", reader.Read(context.Background(), a, false).Error)
	a.ProxyID = nil
	a.Credentials["base_url"] = "https://api.openai.com/v1"
	require.Equal(t, "unsupported", reader.Read(context.Background(), a, false).Status)
	a.Credentials["base_url"] = "https://upstream.example"
	a.Type = AccountTypeOAuth
	require.Equal(t, "unsupported", reader.Read(context.Background(), a, false).Status)
}

func TestAccountWalletAlreadyCancelledDoesNotDispatch(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		t.Fatal("cancelled request must not dispatch")
		return nil, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, "error", reader.Read(ctx, a, false).Status)
}
