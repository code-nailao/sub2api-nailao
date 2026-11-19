package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type walletConfigRepository struct {
	AccountRepository
	calls  int
	config map[string]any
	err    error
}

func (r *walletConfigRepository) UpdateAccountWalletConfig(_ context.Context, _ *Account, config map[string]any) error {
	r.calls++
	r.config = config
	return r.err
}

func TestWalletURLOnlyAcceptsPublicHTTPSRoot(t *testing.T) {
	for raw, want := range map[string]string{
		" https://Sub.Example.COM/v1/ ":   "https://sub.example.com",
		"https://sub.example.com:443/":    "https://sub.example.com",
		"https://sub.example.com:8443/v1": "https://sub.example.com:8443",
	} {
		got, err := NormalizeAccountWalletURL(raw)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"", "http://example.com", "https://localhost", "https://foo.localhost", "https://127.0.0.1", "https://10.0.0.1", "https://[::1]", "https://example.com./", "https://user:secret@example.com", "https://example.com/path", "https://example.com/v1/usage", "https://example.com?secret=1", "https://example.com?", "https://example.com#fragment", "https://example.com/%76%31", "https://example.com:99999", strings.Repeat("a", 2049)} {
		_, err := NormalizeAccountWalletURL(raw)
		require.ErrorIs(t, err, ErrWalletURLInvalid, raw)
	}
}

func TestWalletCustomURLUsesSavedKeyAndMinimalHeadersWithoutChangingModelURL(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(r *http.Request, _ string, id int64) (*http.Response, error) {
		require.Equal(t, "https://wallet.example.com/v1/usage?days=1", r.URL.String())
		require.Equal(t, "Bearer private-key", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Host"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-Original-Secret"))
		require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
		require.True(t, HTTPUpstreamPublicHostsOnly(r.Context()))
		require.Equal(t, int64(1), id)
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":0,"unit":"USD"}`), nil
	}})
	a.Credentials["header_overrides_enabled"] = true
	a.Credentials["header_overrides"] = map[string]any{"Authorization": "other-key", "Cookie": "secret", "Host": "direct.example", "X-Original-Secret": "private"}
	repo := &walletConfigRepository{}
	reader.test.accountRepo = repo
	before, _ := json.Marshal(a)
	config := reader.Configuration(a)
	probe, err := reader.ProbeConfiguration(t.Context(), a, "https://wallet.example.com/v1", config.Revision)
	require.NoError(t, err)
	require.Equal(t, "0", probe.Wallet.Remaining)
	require.NotEmpty(t, probe.VerificationToken)
	require.Zero(t, repo.calls, "testing never saves")
	after, _ := json.Marshal(a)
	require.Equal(t, before, after)
	saved, err := reader.SaveConfiguration(t.Context(), a, probe.URL, config.Revision, probe.VerificationToken)
	require.NoError(t, err)
	require.Equal(t, probe.URL, saved.URL)
	require.NotEqual(t, config.Revision, saved.Revision)
	require.Equal(t, "https://upstream.example/v1", a.GetCredential("base_url"))
	require.Equal(t, map[string]any{"url": probe.URL, "version": repo.config["version"]}, repo.config)
	require.NotEmpty(t, repo.config["version"])
	public, _ := json.Marshal(saved)
	require.NotContains(t, string(public), "private-key")
}

func TestWalletSaveRequiresSuccessfulUnexpiredProbeOfExactIdentity(t *testing.T) {
	for _, change := range []string{"missing-proof", "url", "key", "proxy", "configuration", "expired", "forged", "restart", "cas"} {
		t.Run(change, func(t *testing.T) {
			reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
				return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":1,"unit":"USD"}`), nil
			}})
			repo := &walletConfigRepository{}
			reader.test.accountRepo = repo
			revision := reader.Configuration(a).Revision
			probe, err := reader.ProbeConfiguration(t.Context(), a, "https://wallet.example.com", revision)
			require.NoError(t, err)
			target, token := probe.URL, probe.VerificationToken
			switch change {
			case "missing-proof":
				token = ""
			case "url":
				target = "https://other.example.com"
			case "key":
				a.Credentials["api_key"] = "rotated"
			case "proxy":
				id := int64(8)
				a.ProxyID = &id
			case "configuration":
				a.Extra = map[string]any{AccountWalletConfigExtraKey: map[string]any{"url": target, "version": "new"}}
			case "expired":
				expires := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
				token = expires + "." + reader.walletSignature("probe", AccountWalletIdentity(a), target, expires)
			case "forged":
				token += "0"
			case "restart":
				reader = NewAccountWalletReader(reader.test)
			case "cas":
				repo.err = ErrWalletConfigChanged
			}
			_, err = reader.SaveConfiguration(t.Context(), a, target, revision, token)
			require.Error(t, err)
			if change != "cas" {
				require.Zero(t, repo.calls)
			}
		})
	}
}

func TestWalletProbeDoesNotVerifyQuotaOrStaleValueAndConfigSeparatesCache(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		return walletResponse(`{"mode":"quota_limited","isValid":true,"remaining":10,"unit":"USD"}`), nil
	}})
	probe, err := reader.ProbeConfiguration(t.Context(), a, "https://wallet.example.com", reader.Configuration(a).Revision)
	require.NoError(t, err)
	require.Equal(t, "wallet_unavailable", probe.Wallet.Status)
	require.Empty(t, probe.VerificationToken)
	old := AccountWalletIdentity(a)
	a.Extra = map[string]any{AccountWalletConfigExtraKey: map[string]any{"url": "https://wallet.example.com", "version": "one"}}
	require.NotEqual(t, old, AccountWalletIdentity(a))
	repo := &walletConfigRepository{}
	reader.test.accountRepo = repo
	config, err := reader.SaveConfiguration(t.Context(), a, "", reader.Configuration(a).Revision, "")
	require.NoError(t, err)
	require.Empty(t, config.URL)
	require.NotEmpty(t, repo.config["version"], "reset keeps a new version to prevent replay")
}
