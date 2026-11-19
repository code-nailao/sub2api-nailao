package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"golang.org/x/sync/singleflight"
)

const AccountWalletQueryTimeout = 360 * time.Second
const AccountWalletOperationTimeout = AccountWalletQueryTimeout + 15*time.Second
const AccountWalletLeaseDuration = AccountWalletOperationTimeout + 15*time.Second
const AccountWalletRefreshInterval = 5 * time.Minute
const accountWalletConcurrency = 20
const accountWalletSiteConcurrency = 4

// AccountWallet contains only the upstream account wallet, never key/subscription limits.
// Credentials and upstream response bodies never leave this reader.
type AccountWallet struct {
	WalletID         string     `json:"wallet_id,omitempty"`
	WalletName       string     `json:"wallet_name,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	RepresentativeID int64      `json:"representative_id,omitempty"`
	SourceID         string     `json:"source_id,omitempty"`
	SharedCount      int        `json:"shared_count,omitempty"`
	Querying         bool       `json:"querying,omitempty"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	NextCheckAt      *time.Time `json:"next_check_at,omitempty"`
	AutoRefresh      bool       `json:"auto_refresh"`
	QueryURL         string     `json:"query_url,omitempty"`
	Status           string     `json:"status"`
	Remaining        string     `json:"remaining,omitempty"`
	Unit             string     `json:"unit,omitempty"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	Error            string     `json:"error,omitempty"`
	Stale            bool       `json:"stale"`
}
type walletCacheEntry struct {
	value AccountWallet
	at    time.Time
}
type AccountWalletReader struct {
	Manager     *WalletManager
	test        *AccountTestService
	store       AccountWalletSnapshotRepository
	accounts    AccountRepository
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	mu          sync.Mutex
	cache       map[string]walletCacheEntry
	slots       chan struct{}
	siteSlots   map[string]int
	flight      singleflight.Group
	proofKey    [32]byte
	asyncCtx    context.Context
	asyncCancel context.CancelFunc
	asyncJobs   map[string]*walletAsyncJob
	asyncActive int
	closing     bool
}

func NewAccountWalletReader(test *AccountTestService) *AccountWalletReader {
	reader := &AccountWalletReader{test: test, cache: make(map[string]walletCacheEntry), slots: make(chan struct{}, accountWalletConcurrency), siteSlots: make(map[string]int)}
	reader.asyncCtx, reader.asyncCancel = context.WithCancel(context.Background())
	reader.asyncJobs = make(map[string]*walletAsyncJob)
	_, _ = rand.Read(reader.proofKey[:])
	return reader
}

// Bound total queries and avoid sending all twenty requests to one supplier.
func (s *AccountWalletReader) acquireWalletSlot(a *Account) (func(), bool) {
	base := accountWalletCustomURL(a)
	if base == "" {
		base = accountWalletCallURL(a)
	}
	site := ""
	if u, err := url.Parse(base); err == nil {
		site = strings.ToLower(u.Hostname())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.siteSlots[site] >= accountWalletSiteConcurrency {
		return nil, false
	}
	select {
	case s.slots <- struct{}{}:
		s.siteSlots[site]++
	default:
		return nil, false
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		<-s.slots
		s.siteSlots[site]--
		if s.siteSlots[site] == 0 {
			delete(s.siteSlots, site)
		}
	}, true
}

// Bind cached results to every calling credential and proxy setting, not just the ID.
func AccountWalletIdentity(a *Account) string {
	proxyURL := ""
	if a.Proxy != nil {
		proxyURL = a.Proxy.URL()
	}
	b, _ := json.Marshal([]any{a.ID, a.CreatedAt, a.Platform, a.Type, a.ParentAccountID, a.Credentials, a.ProxyID, proxyURL, a.Extra[AccountWalletConfigExtraKey]})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s *AccountWalletReader) Read(ctx context.Context, a *Account, refresh bool) AccountWallet {
	if s.Manager != nil {
		target, wallet, err := s.Manager.Resolve(ctx, a)
		if err != nil {
			if walletRegistryError(err) {
				return AccountWallet{Status: "rebind_required", Error: "wallet_rebind_required"}
			}
			return walletFailure("wallet_snapshot_unavailable")
		}
		if wallet != nil && wallet.Provider == "new" {
			return s.Manager.decorate(ctx, s.Manager.ExternalBalance(ctx, *wallet), wallet)
		}
		return s.Manager.decorate(ctx, s.readRaw(ctx, target, refresh), wallet)
	}
	return s.readRaw(ctx, a, refresh)
}

func (s *AccountWalletReader) readRaw(ctx context.Context, a *Account, refresh bool) AccountWallet {
	if s.store != nil {
		return s.readPersisted(ctx, a, refresh, false)
	}
	return s.readMemory(ctx, a, refresh)
}

func (s *AccountWalletReader) Scope(ctx context.Context, a *Account) (string, error) {
	if s.Manager != nil {
		return s.Manager.Scope(ctx, a)
	}
	return AccountWalletIdentity(a), nil
}

// Candidate probes are deliberately memory-only until their URL is saved.
func (s *AccountWalletReader) readMemory(ctx context.Context, a *Account, refresh bool) AccountWallet {
	if ctx.Err() != nil {
		return walletRequestFailure(ctx, nil, "upstream_unavailable")
	}
	key := AccountWalletIdentity(a)
	read := func() (AccountWallet, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		old, ok := s.cache[key]
		ttl := 5 * time.Minute
		if old.value.Status != "ready" {
			ttl = time.Minute
		}
		if refresh {
			ttl = 15 * time.Second
		}
		return old.value, ok && time.Since(old.at) < ttl
	}
	if v, ok := read(); ok {
		return v
	}
	result := s.flight.DoChan(key, func() (any, error) {
		if v, ok := read(); ok {
			return v, nil
		}
		release, ok := s.acquireWalletSlot(a)
		if !ok {
			return walletFailure("refresh_in_progress_or_rate_limited"), nil
		}
		defer release()
		call, cancel := context.WithTimeout(ctx, AccountWalletQueryTimeout)
		defer cancel()
		v := s.fetch(call, a)
		v.QueryURL = accountWalletCustomURL(a)
		if ctx.Err() != nil {
			return v, nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if old, ok := s.cache[key]; ok && v.Status == "error" && old.value.Remaining != "" {
			v.Remaining = old.value.Remaining
			v.Unit = old.value.Unit
			v.CheckedAt = old.value.CheckedAt
			v.Stale = true
		}
		if len(s.cache) >= 512 {
			var oldest string
			var at time.Time
			for k, e := range s.cache {
				if oldest == "" || e.at.Before(at) {
					oldest = k
					at = e.at
				}
			}
			delete(s.cache, oldest)
		}
		s.cache[key] = walletCacheEntry{value: v, at: time.Now()}
		return v, nil
	})
	select {
	case <-ctx.Done():
		return walletRequestFailure(ctx, nil, "upstream_unavailable")
	case r := <-result:
		value, ok := r.Val.(AccountWallet)
		if r.Err != nil || !ok {
			return walletRequestFailure(ctx, nil, "upstream_unavailable")
		}
		return value
	}
}
func walletRequestFailure(ctx context.Context, err error, fallback string) AccountWallet {
	var timeout net.Error
	if !errors.Is(ctx.Err(), context.Canceled) && (errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()) {
		return walletFailure("upstream_timeout")
	}
	return walletFailure(fallback)
}

func walletFailure(code string) AccountWallet { return AccountWallet{Status: "error", Error: code} }
func (s *AccountWalletReader) fetch(ctx context.Context, a *Account) AccountWallet {
	if !walletAccountSupported(a) {
		return AccountWallet{Status: "unsupported"}
	}
	base := accountWalletCallURL(a)
	custom := accountWalletCustomURL(a)
	if custom != "" {
		var err error
		base, err = NormalizeAccountWalletURL(custom)
		if err != nil {
			return walletFailure("invalid_base_url")
		}
		ctx = WithHTTPUpstreamPublicHostsOnly(ctx)
	}
	if base == "" || upstreamBillingProbeTargetIsOfficialAPI(base) {
		return AccountWallet{Status: "unsupported"}
	}
	if s.test == nil || s.test.httpUpstream == nil {
		return walletRequestFailure(ctx, nil, "upstream_unavailable")
	}
	base, err := s.test.validateUpstreamBaseURL(base)
	if err != nil {
		return walletFailure("invalid_base_url")
	}
	proxy := ""
	if a.ProxyID != nil {
		if a.Proxy == nil || a.Proxy.ID != *a.ProxyID {
			return walletFailure("proxy_unavailable")
		}
		proxy = a.Proxy.URL()
	}
	request, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodGet, buildOpenAIEndpointURL(base, "/v1/usage")+"?days=1", nil)
	if err != nil {
		return walletFailure("invalid_base_url")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+a.GetCredential("api_key"))
	if custom == "" {
		a.ApplyHeaderOverrides(request.Header)
	}
	// The same native HTTP client supplies SSRF checks, proxy routing and account transport limits.
	request = request.WithContext(WithHTTPUpstreamProfile(request.Context(), HTTPUpstreamProfileWallet))
	var tlsProfile *tlsfingerprint.Profile
	if s.test.tlsFPProfileService != nil {
		tlsProfile = s.test.tlsFPProfileService.ResolveTLSProfile(a)
	}
	response, err := s.test.httpUpstream.DoWithTLS(request, proxy, a.ID, a.Concurrency, tlsProfile)
	if err != nil || response == nil || response.Body == nil {
		return walletRequestFailure(ctx, err, "upstream_unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case 401, 403:
		return walletFailure("upstream_credential_invalid")
	case 404, 405:
		return AccountWallet{Status: "unsupported"}
	}
	if response.StatusCode != http.StatusOK {
		return walletRequestFailure(ctx, nil, "upstream_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return walletRequestFailure(ctx, err, "upstream_response_invalid")
	}
	return parseAccountWallet(raw, time.Now().UTC())
}

var walletNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func exactWalletAmount(n json.Number) (string, bool) {
	raw := string(n)
	if len(raw) > 64 || !walletNumber.MatchString(raw) {
		return "", false
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		e, err := strconv.Atoi(raw[i+1:])
		if err != nil || e < -32 || e > 32 {
			return "", false
		}
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return "", false
	}
	// Bound hostile exponents before formatting; preserve every accepted decimal exactly.
	if r.Num().BitLen() > 256 || r.Denom().BitLen() > 256 {
		return "", false
	}
	value := strings.TrimRight(strings.TrimRight(r.FloatString(32), "0"), ".")
	check, ok := new(big.Rat).SetString(value)
	if !ok || check.Cmp(r) != 0 {
		return "", false
	}
	if value == "-0" {
		value = "0"
	}
	return value, true
}
func parseAccountWallet(raw []byte, now time.Time) AccountWallet {
	var value struct {
		Mode         string          `json:"mode"`
		Valid        *bool           `json:"isValid"`
		Balance      json.Number     `json:"balance"`
		Remaining    json.Number     `json:"remaining"`
		Unit         string          `json:"unit"`
		Subscription json.RawMessage `json:"subscription"`
		Quota        json.RawMessage `json:"quota"`
		RateLimits   json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Valid == nil || !*value.Valid {
		return walletFailure("upstream_response_invalid")
	}
	if value.Mode == "quota_limited" {
		return AccountWallet{Status: "wallet_unavailable"}
	}
	if value.Mode != "unrestricted" {
		return walletFailure("upstream_response_invalid")
	}
	// Only an explicit balance is a wallet. Remaining/planName alone is not sufficient.
	if len(value.Subscription) > 0 && string(value.Subscription) != "null" || value.Balance == "" {
		return AccountWallet{Status: "wallet_unavailable"}
	}
	if len(value.Quota) > 0 && string(value.Quota) != "null" || len(value.RateLimits) > 0 && string(value.RateLimits) != "null" {
		return walletFailure("upstream_response_invalid")
	}
	balance, ok := exactWalletAmount(value.Balance)
	if !ok {
		return walletFailure("upstream_response_invalid")
	}
	if value.Remaining != "" {
		remaining, ok := exactWalletAmount(value.Remaining)
		if !ok || remaining != balance {
			return walletFailure("upstream_response_invalid")
		}
	}
	if value.Unit != "USD" {
		return walletFailure("upstream_unit_unconfirmed")
	}
	return AccountWallet{Status: "ready", Remaining: balance, Unit: value.Unit, CheckedAt: &now}
}
