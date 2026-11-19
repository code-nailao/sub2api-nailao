package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func fixtureProvider(t *testing.T, kind string, quota string) Provider {
	t.Helper()
	return Provider{Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Fatalf("mutation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/user/self":
			if r.Header.Get("Authorization") != "Bearer personal-fixture" {
				t.Fatal("wrong credential")
			}
			return reply(200, `{"success":true,"data":{"id":42,"quota":`+quota+`,"used_quota":500000}}`), nil
		case "/api/status":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("credential sent to public metadata")
			}
			body, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"quota_per_unit": 500000, "quota_display_type": kind, "usd_exchange_rate": 6.5, "custom_currency_symbol": "积分", "custom_currency_exchange_rate": 2}})
			return reply(200, string(body)), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, nil
		}
	})}}
}
func TestAccountBalanceUnits(t *testing.T) {
	for _, test := range []struct{ kind, want, unit string }{{"USD", "2.5", "USD"}, {"CNY", "16.25", "CNY"}, {"TOKENS", "1250000", "TOKENS"}, {"CUSTOM", "5", "积分"}} {
		t.Run(test.kind, func(t *testing.T) {
			b, err := fixtureProvider(t, test.kind, "1250000").Probe(context.Background(), "https://vendor.example", "personal-fixture", 42)
			if err != nil || b.Remaining != test.want || b.Unit != test.unit || b.AccountID != 42 {
				t.Fatalf("%+v %v", b, err)
			}
		})
	}
}
func TestBalanceRejectsModelKeyAndAccountChange(t *testing.T) {
	p := fixtureProvider(t, "USD", "1250000")
	if _, err := p.Probe(context.Background(), "https://vendor.example", "sk-model-key", 0); !errors.Is(err, ErrCredential) {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), "https://vendor.example", "personal-fixture", 43); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}
func TestBalanceFailuresDoNotBecomeZero(t *testing.T) {
	for _, body := range []string{`<html>denied</html>`, `{"success":false}`, `{"success":true,"data":{"id":42,"quota":"NaN","used_quota":0}}`, `{"success":true,"data":{"id":42,"quota":1e9999999,"used_quota":0}}`, strings.Repeat("x", (1<<20)+1)} {
		p := Provider{Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return reply(200, body), nil })}}
		if _, err := p.Probe(context.Background(), "https://vendor.example", "personal-fixture", 0); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	for _, status := range []int{301, 401, 403, 404, 429, 500} {
		p := Provider{Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return reply(status, "secret-provider-response"), nil })}}
		if _, err := p.Probe(context.Background(), "https://vendor.example", "personal-fixture", 0); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	if _, err := fixtureProvider(t, "UNKNOWN", "1250000").Probe(context.Background(), "https://vendor.example", "personal-fixture", 0); !errors.Is(err, ErrUnit) {
		t.Fatal(err)
	}
}
func TestOnlyPublicOriginsAndNoRedirect(t *testing.T) {
	for _, origin := range []string{"http://vendor.example", "https://user:pass@vendor.example", "https://vendor.example/v1", "https://vendor.example?q=secret", "https://localhost", "https://127.0.0.1", "https://[::1]", "https://169.254.169.254"} {
		if _, err := CanonicalOrigin(origin); err == nil {
			t.Fatal(origin)
		}
	}
	for _, ip := range []string{"10.0.0.1", "100.64.1.2", "192.168.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "2001:db8::1", "64:ff9b::7f00:1", "2002:7f00:1::1"} {
		if publicAddress(netip.MustParseAddr(ip)) {
			t.Fatal(ip)
		}
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public IP rejected")
	}
	c := PublicClient()
	if c.CheckRedirect(httptest.NewRequest("GET", "https://another.example", nil), nil) != http.ErrUseLastResponse {
		t.Fatal("redirect followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Transport.(*http.Transport).DialContext(ctx, "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("loopback dial allowed")
	}
}
func TestPersonalTokenLegacyUserHeader(t *testing.T) {
	p := fixtureProvider(t, "USD", "0")
	original := p.Client.Transport
	p.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/user/self" && r.Header.Get("New-Api-User") != "42" {
			t.Fatal("missing legacy user id")
		}
		return original.RoundTrip(r)
	})
	b, err := p.Probe(context.Background(), "https://vendor.example", "personal-fixture", 42)
	if err != nil || b.Remaining != "0" {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestBalanceReportsRequiredNewAPIUserID(t *testing.T) {
	for _, message := range []string{
		"Unauthorized, New-Api-User header not provided",
		"无权进行此操作，未提供 New-Api-User",
	} {
		t.Run(message, func(t *testing.T) {
			p := Provider{Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"success": false, "message": message})
				return reply(http.StatusUnauthorized, string(body)), nil
			})}}
			_, err := p.Probe(context.Background(), "https://vendor.example", "personal-fixture", 0)
			if err == nil || err.Error() != "upstream_user_id_required" {
				t.Fatalf("missing user ID was misreported: %v", err)
			}
		})
	}
}

func TestBalanceMissingUserIDCanBeCompletedWithoutReplacingToken(t *testing.T) {
	svc := newTestService(t)
	original := svc.Provider.Client.Transport
	svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/user/self" && r.Header.Get("New-Api-User") == "" {
			return reply(401, `{"success":false,"message":"Unauthorized, New-Api-User header not provided"}`), nil
		}
		return original.RoundTrip(r)
	})
	in := input()
	in.AccountID = 0
	source, err := svc.Save("", in, 9)
	if err != nil {
		t.Fatal(err)
	}
	source, err = svc.Refresh(context.Background(), source.ID)
	if err != nil || source.Error != "upstream_user_id_required" || source.LastBalance != nil {
		t.Fatalf("unverified source: status=%s error=%s err=%v", source.Status, source.Error, err)
	}
	in.Revision, in.AccountID, in.PersonalToken = source.Revision, 42, ""
	if _, err = svc.Save(source.ID, in, 9); err != nil {
		t.Fatal(err)
	}
	resetLimit(svc, source.ID)
	source, err = svc.Refresh(context.Background(), source.ID)
	if err != nil || source.AccountID != 42 || source.LastBalance == nil || source.LastBalance.Remaining != "2.5" {
		t.Fatalf("user ID correction did not verify the saved token: %v", err)
	}
}

func TestOriginCanonicalizationPreventsDuplicateWalletAliases(t *testing.T) {
	for _, raw := range []string{"https://Vendor.Example:443/", "HTTPS://vendor.example/", "https://vendor.example."} {
		origin, err := CanonicalOrigin(raw)
		if err != nil || origin != "https://vendor.example" {
			t.Fatalf("%s -> %s %v", raw, origin, err)
		}
	}
}
