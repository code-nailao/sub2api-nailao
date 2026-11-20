package portal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 对外只返回固定错误码，不输出上游原始响应或凭据。
var (
	ErrCredential     = errors.New("upstream_credential_invalid")
	ErrUserIDRequired = errors.New("upstream_user_id_required")
	ErrTransport      = errors.New("upstream_unavailable")
	ErrResponse       = errors.New("upstream_response_invalid")
	ErrUnit           = errors.New("upstream_unit_unconfirmed")
	ErrIdentity       = errors.New("upstream_account_changed")
)

type Balance struct {
	AccountID int64     `json:"account_id"`
	Remaining string    `json:"remaining"`
	Used      string    `json:"used"`
	Unit      string    `json:"unit"`
	CheckedAt time.Time `json:"checked_at"`
}
type Provider struct{ Client *http.Client }

func CanonicalOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(u.Host, "\\\r\n") {
		return "", errors.New("https_site_origin_required")
	}
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil && !publicAddress(addr) {
		return "", errors.New("public_site_required")
	}
	if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "localhost") {
		return "", errors.New("public_site_required")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("https_site_origin_required")
		}
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" && port != "443" {
		authority = net.JoinHostPort(host, port)
	}
	return "https://" + authority, nil
}
func publicAddress(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32"} {
		if netip.MustParsePrefix(p).Contains(a) {
			return false
		}
	}
	return true
}

// 对同一批已验证公网IP直接拨号，避免DNS重绑定；不跟随重定向发送个人令牌。
func PublicClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 16}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrTransport
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, ErrTransport
		}
		for _, ip := range ips {
			if !publicAddress(ip) {
				return nil, ErrTransport
			}
		}
		for _, ip := range ips {
			if conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port)); err == nil {
				return conn, nil
			}
		}
		return nil, ErrTransport
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// BalanceClient changes only wallet requests; authentication and other Portal
// clients keep their existing deadlines.
const BalanceQueryTimeout = 360 * time.Second
const balanceOperationTimeout = BalanceQueryTimeout + 15*time.Second

func BalanceClient() *http.Client {
	client := PublicClient()
	transport := client.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = BalanceQueryTimeout
	client.Timeout = BalanceQueryTimeout
	return client
}

var ErrQueryTimeout = errors.New("upstream_timeout")

func balanceQueryError(ctx context.Context, err, fallback error) error {
	if errors.Is(context.Cause(ctx), errBalanceShutdown) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, errBalanceShutdown)) {
		return errBalanceShutdown
	}
	var timeout net.Error
	if !errors.Is(ctx.Err(), context.Canceled) && (errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()) {
		return ErrQueryTimeout
	}
	return fallback
}

func getJSON(ctx context.Context, client *http.Client, endpoint, token string, userID int64, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if userID > 0 {
		req.Header.Set("New-Api-User", strconv.FormatInt(userID, 10))
	}
	res, err := client.Do(req)
	if err != nil {
		return balanceQueryError(ctx, err, ErrTransport)
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		// Some New API sites require a separate user ID even for a valid PAT.
		// Classify only the known missing-header response; never expose its body.
		if userID == 0 {
			var failure struct {
				Message string `json:"message"`
			}
			body, readErr := io.ReadAll(io.LimitReader(res.Body, 8193))
			if readErr == nil && len(body) <= 8192 && json.Unmarshal(body, &failure) == nil {
				message := strings.ToLower(failure.Message)
				if strings.Contains(message, "new-api-user") &&
					(strings.Contains(message, "not provided") || strings.Contains(message, "missing") ||
						strings.Contains(message, "未提供") || strings.Contains(message, "缺少")) {
					return ErrUserIDRequired
				}
			}
		}
		return ErrCredential
	}
	if res.StatusCode != 200 {
		return ErrTransport
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return balanceQueryError(ctx, err, ErrResponse)
	}
	if err = json.Unmarshal(body, target); err != nil {
		return ErrResponse
	}
	return nil
}

var decimalPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]{0,23})(?:\.[0-9]{1,16})?(?:[eE][+-]?[0-9]{1,2})?$`)

func amountRat(raw string) (*big.Rat, bool) {
	if len(raw) > 64 || !decimalPattern.MatchString(raw) {
		return nil, false
	}
	return new(big.Rat).SetString(raw)
}
func positiveRat(n json.Number) (*big.Rat, error) {
	x, ok := amountRat(string(n))
	if !ok || x.Sign() <= 0 {
		return nil, ErrUnit
	}
	return x, nil
}

func decimal(x *big.Rat) string {
	s := strings.TrimRight(strings.TrimRight(x.FloatString(8), "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}
func (p Provider) Probe(ctx context.Context, base, pat string, expectedID int64) (Balance, error) {
	ctx, cancel := context.WithTimeout(ctx, BalanceQueryTimeout)
	defer cancel()
	var zero Balance
	if strings.TrimSpace(pat) == "" || strings.HasPrefix(pat, "sk-") || strings.ContainsAny(pat, "\r\n") {
		return zero, ErrCredential
	}
	origin, err := CanonicalOrigin(base)
	if err != nil {
		return zero, err
	}
	var self struct {
		Success bool `json:"success"`
		Data    struct {
			ID    int64       `json:"id"`
			Quota json.Number `json:"quota"`
			Used  json.Number `json:"used_quota"`
		} `json:"data"`
	}
	if err = getJSON(ctx, p.Client, origin+"/api/user/self", pat, expectedID, &self); err != nil {
		return zero, err
	}
	if !self.Success || self.Data.ID <= 0 {
		return zero, ErrResponse
	}
	if expectedID > 0 && self.Data.ID != expectedID {
		return zero, ErrIdentity
	}
	remaining, ok := amountRat(string(self.Data.Quota))
	if !ok {
		return zero, ErrResponse
	}
	used, ok := amountRat(string(self.Data.Used))
	if !ok || used.Sign() < 0 {
		return zero, ErrResponse
	}
	var status struct {
		Success bool `json:"success"`
		Data    struct {
			PerUnit    json.Number `json:"quota_per_unit"`
			Kind       string      `json:"quota_display_type"`
			Rate       json.Number `json:"usd_exchange_rate"`
			Symbol     string      `json:"custom_currency_symbol"`
			CustomRate json.Number `json:"custom_currency_exchange_rate"`
		} `json:"data"`
	}
	if err = getJSON(ctx, p.Client, origin+"/api/status", "", 0, &status); err != nil {
		return zero, err
	}
	if !status.Success {
		return zero, ErrUnit
	}
	factor := big.NewRat(1, 1)
	unit := status.Data.Kind
	if unit != "TOKENS" {
		per, err := positiveRat(status.Data.PerUnit)
		if err != nil {
			return zero, err
		}
		factor.Inv(per)
	}
	switch unit {
	case "USD", "TOKENS":
	case "CNY":
		rate, err := positiveRat(status.Data.Rate)
		if err != nil {
			return zero, err
		}
		factor.Mul(factor, rate)
	case "CUSTOM":
		rate, err := positiveRat(status.Data.CustomRate)
		if err != nil {
			return zero, err
		}
		unit = strings.TrimSpace(status.Data.Symbol)
		if unit == "" || len(unit) > 16 {
			return zero, ErrUnit
		}
		factor.Mul(factor, rate)
	default:
		return zero, ErrUnit
	}
	return Balance{AccountID: self.Data.ID, Remaining: decimal(remaining.Mul(remaining, factor)), Used: decimal(used.Mul(used, factor)), Unit: unit, CheckedAt: time.Now().UTC()}, nil
}
