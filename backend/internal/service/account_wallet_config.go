package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/google/uuid"
)

// This server-managed subtree is only written by the wallet configuration CAS.
// Generic account edits must preserve the current value under their row lock.
const AccountWalletConfigExtraKey = "sources_wallet_query"

var (
	ErrWalletConfigChanged = infraerrors.New(http.StatusConflict, "wallet_config_changed", "Account or balance configuration changed; reload and test again")
	ErrWalletURLInvalid    = infraerrors.BadRequest("wallet_url_invalid", "Use a public HTTPS site root or /v1 URL")
	ErrWalletNotVerified   = infraerrors.BadRequest("wallet_not_verified", "Test this balance address again before saving")
	ErrWalletUnsupported   = infraerrors.BadRequest("wallet_unsupported", "This account does not support API-key wallet queries")
)

type AccountWalletConfigRepository interface {
	UpdateAccountWalletConfig(context.Context, *Account, map[string]any) error
}

type AccountWalletConfig struct {
	URL       string `json:"url"`
	CallURL   string `json:"call_url"`
	Revision  string `json:"revision"`
	Supported bool   `json:"supported"`
}

type AccountWalletProbe struct {
	URL               string        `json:"url"`
	Wallet            AccountWallet `json:"wallet"`
	VerificationToken string        `json:"verification_token,omitempty"`
}

func walletAccountSupported(a *Account) bool {
	return a != nil && a.Type == AccountTypeAPIKey && a.Platform != PlatformAntigravity && a.ParentAccountID == nil && a.GetCredential("api_key") != ""
}

func accountWalletCallURL(a *Account) string {
	if a.RoutesProtocolByInbound() && a.IsAdaptiveAPIProtocol() {
		return a.GetCNProtocolBaseURL(APIProtocolChatCompletions)
	}
	return a.GetCredential("base_url")
}

func accountWalletCustomURL(a *Account) string {
	config, _ := a.Extra[AccountWalletConfigExtraKey].(map[string]any)
	value, _ := config["url"].(string)
	return value
}

// NormalizeAccountWalletURL deliberately accepts only the fixed Sub wallet contract.
// Network-time DNS checks are enforced by WithHTTPUpstreamPublicHostsOnly as well.
func NormalizeAccountWalletURL(raw string) (string, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, "?#\\") {
		return "", ErrWalletURLInvalid
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" {
		return "", ErrWalletURLInvalid
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/v1" && u.Path != "/v1/" {
		return "", ErrWalletURLInvalid
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if strings.HasSuffix(u.Hostname(), ".") || !strings.Contains(u.Hostname(), ".") && !strings.Contains(u.Hostname(), ":") {
		return "", ErrWalletURLInvalid
	}
	u.Path = ""
	if u.Port() == "443" {
		u.Host = strings.TrimSuffix(u.Host, ":443")
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(u.String(), urlvalidator.ValidationOptions{})
	if err != nil {
		return "", ErrWalletURLInvalid
	}
	return normalized, nil
}

func (s *AccountWalletReader) walletSignature(parts ...string) string {
	h := hmac.New(sha256.New, s.proofKey[:])
	b, _ := json.Marshal(parts)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *AccountWalletReader) Configuration(a *Account) AccountWalletConfig {
	// The display URL must not reveal embedded credentials, query strings or fragments.
	callURL := ""
	if u, err := url.Parse(accountWalletCallURL(a)); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		u.RawFragment = ""
		callURL = u.String()
	}
	return AccountWalletConfig{URL: accountWalletCustomURL(a), CallURL: callURL,
		Revision: s.walletSignature("revision", AccountWalletIdentity(a)), Supported: walletAccountSupported(a)}
}

func walletCandidate(a *Account, target string) *Account {
	copy := *a
	copy.Extra = make(map[string]any, len(a.Extra)+1)
	for k, v := range a.Extra {
		copy.Extra[k] = v
	}
	copy.Extra[AccountWalletConfigExtraKey] = map[string]any{"url": target, "version": "probe"}
	return &copy
}

func (s *AccountWalletReader) ProbeConfiguration(ctx context.Context, a *Account, rawURL, revision string) (AccountWalletProbe, error) {
	if !walletAccountSupported(a) {
		return AccountWalletProbe{}, ErrWalletUnsupported
	}
	if !hmac.Equal([]byte(revision), []byte(s.Configuration(a).Revision)) {
		return AccountWalletProbe{}, ErrWalletConfigChanged
	}
	target, err := NormalizeAccountWalletURL(rawURL)
	if err != nil {
		return AccountWalletProbe{}, err
	}
	wallet := s.readMemory(ctx, walletCandidate(a, target), true)
	result := AccountWalletProbe{URL: target, Wallet: wallet}
	if wallet.Status == "ready" && !wallet.Stale && wallet.CheckedAt != nil {
		expires := strconv.FormatInt(wallet.CheckedAt.Add(5*time.Minute).Unix(), 10)
		result.VerificationToken = expires + "." + s.walletSignature("probe", AccountWalletIdentity(a), target, expires)
	}
	return result, nil
}

func (s *AccountWalletReader) SaveConfiguration(ctx context.Context, a *Account, rawURL, revision, token string) (AccountWalletConfig, error) {
	if !hmac.Equal([]byte(revision), []byte(s.Configuration(a).Revision)) {
		return AccountWalletConfig{}, ErrWalletConfigChanged
	}
	target := ""
	if rawURL != "" {
		if !walletAccountSupported(a) {
			return AccountWalletConfig{}, ErrWalletUnsupported
		}
		var err error
		target, err = NormalizeAccountWalletURL(rawURL)
		if err != nil {
			return AccountWalletConfig{}, err
		}
		expires, signature, ok := strings.Cut(token, ".")
		deadline, err := strconv.ParseInt(expires, 10, 64)
		if !ok || err != nil || deadline <= time.Now().Unix() || deadline > time.Now().Add(5*time.Minute).Unix() ||
			!hmac.Equal([]byte(signature), []byte(s.walletSignature("probe", AccountWalletIdentity(a), target, expires))) {
			return AccountWalletConfig{}, ErrWalletNotVerified
		}
	}
	if s.test == nil {
		return AccountWalletConfig{}, infraerrors.New(http.StatusServiceUnavailable, "wallet_unavailable", "Balance configuration unavailable")
	}
	repo, ok := s.test.accountRepo.(AccountWalletConfigRepository)
	if !ok {
		return AccountWalletConfig{}, infraerrors.New(http.StatusServiceUnavailable, "wallet_unavailable", "Balance configuration unavailable")
	}
	config := map[string]any{"url": target, "version": uuid.NewString()}
	if err := repo.UpdateAccountWalletConfig(ctx, a, config); err != nil {
		return AccountWalletConfig{}, err
	}
	updated := walletCandidate(a, target)
	updated.Extra[AccountWalletConfigExtraKey] = config
	return s.Configuration(updated), nil
}
