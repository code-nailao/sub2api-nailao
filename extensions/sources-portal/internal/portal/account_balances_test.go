package portal

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func balanceFixture(t *testing.T) (*Service, Source, string) {
	t.Helper()
	svc := newTestService(t)
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Store.change(func(rows map[string]record) error {
		rec := rows[src.ID]
		rec.Source.AccountID = 42
		rec.Source.Status = "ok"
		rec.Source.LastBalance = &Balance{AccountID: 42, Remaining: "0", Used: "2.10", Unit: "CNY", CheckedAt: time.Now()}
		rows[src.ID] = rec
		src = rec.Source
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, src, "v1:" + strings.Repeat("a", 64)
}
func TestBalanceBindingsShareWalletAndKeepRevisionAcrossRestart(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	origin := "https://main.example"
	for _, id := range []int64{1, 2} {
		if err := svc.Store.setBinding(origin, id, BalanceAccountIdentity{id, true, marker}, src.ID, src.Revision, 0); err != nil {
			t.Fatal(err)
		}
	}
	identities := map[int64]BalanceAccountIdentity{1: {1, true, marker}, 2: {2, true, marker}}
	rows := svc.Store.summaries(origin, []int64{1, 2}, identities, time.Now())
	if rows[0].Balance.Remaining != "0" || rows[0].Balance.Unit != "CNY" || rows[0].SharedCount != 2 || rows[1].BindingRevision != 1 {
		t.Fatalf("%+v", rows)
	}
	if err := svc.Delete(src.ID, src.Revision); !errors.Is(err, ErrSourceLinked) {
		t.Fatal(err)
	}
	if err := svc.Store.setBinding(origin, 1, identities[1], "", 0, 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := svc.Store.setBinding(origin, 1, identities[1], "", 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.loadBindings(); err != nil {
		t.Fatal(err)
	}
	rows = svc.Store.summaries(origin, []int64{1, 2}, identities, time.Now())
	if rows[0].Status != "unbound" || rows[0].BindingRevision != 2 || rows[1].SharedCount != 1 {
		t.Fatalf("%+v", rows)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(svc.Store.path), "account-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "personal-fixture") || strings.Contains(string(raw), "encrypted_token") {
		t.Fatal("binding contains upstream credentials")
	}
	other := svc.Store.summaries("https://another.example", []int64{2}, identities, time.Now())
	if other[0].Status != "unbound" {
		t.Fatal(other)
	}
}
func TestBalanceStaleFailuresAndIdentityChanges(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	origin := "https://main.example"
	id := BalanceAccountIdentity{1, true, marker}
	if err := svc.Store.setBinding(origin, 1, id, src.ID, src.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.change(func(rows map[string]record) error {
		r := rows[src.ID]
		r.Source.Status = "error"
		r.Source.Error = "upstream_unavailable"
		r.Source.LastBalance.CheckedAt = time.Now().Add(-time.Hour)
		rows[src.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows := svc.Store.summaries(origin, []int64{1}, map[int64]BalanceAccountIdentity{1: id}, time.Now())
	if rows[0].Balance == nil || rows[0].Balance.Remaining != "0" || !rows[0].Stale || rows[0].Status != "error" {
		t.Fatal(rows)
	}
	id.Marker = "v1:" + strings.Repeat("b", 64)
	rows = svc.Store.summaries(origin, []int64{1}, map[int64]BalanceAccountIdentity{1: id}, time.Now())
	if rows[0].Status != "rebind_required" || rows[0].Balance != nil {
		t.Fatal(rows)
	}
	id.Supported = false
	rows = svc.Store.summaries(origin, []int64{1, 9}, map[int64]BalanceAccountIdentity{1: id}, time.Now())
	if rows[0].Status != "unsupported" || rows[1].Status != "not_found" {
		t.Fatal(rows)
	}
}
func TestBalanceBindingRequiresVerifiedWalletAndKnownUnit(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	id := BalanceAccountIdentity{1, true, marker}
	if err := svc.Store.change(func(rows map[string]record) error {
		r := rows[src.ID]
		r.Source.Status = "pending"
		rows[src.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.setBinding("https://main.example", 1, id, src.ID, src.Revision, 0); !errors.Is(err, ErrUnverifiedSource) {
		t.Fatal(err)
	}
}
func TestBalanceAPIUsesMainIdentityAndDeduplicatesRefresh(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	s := testServer(t, "admin")
	s.Service = svc
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"admin","status":"active"}}`), nil
		case "/api/v1/admin/accounts/balance-identities":
			if r.Method != "GET" || r.URL.Query().Get("ids") != "1,2" {
				t.Errorf("unexpected identity request %s", r.URL)
			}
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"accounts": []BalanceAccountIdentity{{1, true, marker}, {2, true, marker}}}})
			return reply(200, string(raw)), nil
		default:
			t.Errorf("unreviewed route %s", r.URL)
			return reply(404, ""), nil
		}
	})
	for _, id := range []int64{1, 2} {
		if err := svc.Store.setBinding(s.SubOrigin, id, BalanceAccountIdentity{id, true, marker}, src.ID, src.Revision, 0); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	svc.Provider.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) { calls.Add(1); return reply(503, `{}`), nil })}
	res := request(s, "POST", "/api/balance/accounts/refresh", `{"account_ids":[1,2]}`, "fixture-admin", s.PublicOrigin)
	if res.Code != 200 {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("same wallet queried %d times", calls.Load())
	}
	var out struct {
		Accounts []AccountBalanceSummary `json:"accounts"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 || out.Accounts[0].Balance == nil || out.Accounts[0].Status != "error" {
		t.Fatal(out)
	}
}
func TestBalanceEndpointsRejectOrdinaryUsersAndInvalidIDs(t *testing.T) {
	s := testServer(t, "user")
	for _, req := range []struct{ method, path, body string }{{"GET", "/api/balance/accounts?ids=1", ""}, {"PUT", "/api/balance/accounts/1/binding", `{"source_id":"x","binding_revision":0}`}, {"POST", "/api/balance/accounts/refresh", `{"account_ids":[1]}`}} {
		if got := request(s, req.method, req.path, req.body, "fixture-admin", s.PublicOrigin); got.Code != 403 {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
	}
	for _, raw := range []string{"", "0", "01", "-1", "1,1", "1,2&x=1"} {
		if _, err := parseBalanceIDs(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestBindingsDoNotRewriteSourcesAndReopenWithCredentials(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	before, err := os.ReadFile(svc.Store.path)
	if err != nil {
		t.Fatal(err)
	}
	id := BalanceAccountIdentity{1, true, marker}
	if err = svc.Store.setBinding("https://main.example", 1, id, src.ID, src.Revision, 0); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(svc.Store.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("binding rewrote existing source file", err)
	}
	dir := filepath.Dir(svc.Store.path)
	info, err := os.Stat(filepath.Join(dir, "account-bindings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("binding permissions", err)
	}
	if err = svc.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	token, err := reopened.decrypt(reopened.snapshot()[src.ID])
	if err != nil || token != "personal-fixture" {
		t.Fatal("existing encrypted credentials not retained", err)
	}
	rows := reopened.summaries("https://main.example", []int64{1}, map[int64]BalanceAccountIdentity{1: id}, time.Now())
	if rows[0].Status != "ready" || rows[0].Balance.Remaining != "0" {
		t.Fatal(rows)
	}
}

func TestBindingAndSourceDeletionCannotLeaveDanglingAssociation(t *testing.T) {
	for i := 0; i < 20; i++ {
		svc, src, marker := balanceFixture(t)
		start := make(chan struct{})
		var bindErr, deleteErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			bindErr = svc.Store.setBinding("https://main.example", 1, BalanceAccountIdentity{1, true, marker}, src.ID, src.Revision, 0)
		}()
		go func() { defer wg.Done(); <-start; deleteErr = svc.Delete(src.ID, src.Revision) }()
		close(start)
		wg.Wait()
		if bindErr == nil {
			if !errors.Is(deleteErr, ErrSourceLinked) {
				t.Fatalf("bound source deleted: %v", deleteErr)
			}
		} else if !errors.Is(bindErr, ErrNotFound) || deleteErr != nil {
			t.Fatalf("unexpected errors: %v %v", bindErr, deleteErr)
		}
	}
}

func TestSameSiteDifferentWalletsStaySeparate(t *testing.T) {
	svc, first, marker := balanceFixture(t)
	in := input()
	in.Name = "Other wallet"
	in.AccountID = 99
	in.PersonalToken = "other-personal-token"
	second, err := svc.Save("", in, 9)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.change(func(rows map[string]record) error {
		r := rows[second.ID]
		r.Source.Status = "ok"
		r.Source.LastBalance = &Balance{AccountID: 99, Remaining: "15.25", Unit: "EUR", CheckedAt: time.Now()}
		rows[second.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for account, source := range map[int64]Source{1: first, 2: second} {
		if err = svc.Store.setBinding("https://main.example", account, BalanceAccountIdentity{account, true, marker}, source.ID, source.Revision, 0); err != nil {
			t.Fatal(err)
		}
	}
	rows := svc.Store.summaries("https://main.example", []int64{1, 2}, map[int64]BalanceAccountIdentity{1: {1, true, marker}, 2: {2, true, marker}}, time.Now())
	if rows[0].SourceID == rows[1].SourceID || rows[0].Balance.Unit != "CNY" || rows[1].Balance.Unit != "EUR" || rows[0].SharedCount != 1 || rows[1].SharedCount != 1 {
		t.Fatal(rows)
	}
}

func TestBindingRejectsMissingCurrencyAndStaleSourceRevision(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	id := BalanceAccountIdentity{1, true, marker}
	if err := svc.Store.setBinding("https://main.example", 1, id, src.ID, src.Revision+1, 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := svc.Store.change(func(rows map[string]record) error {
		r := rows[src.ID]
		r.Source.LastBalance.Unit = ""
		rows[src.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.setBinding("https://main.example", 1, id, src.ID, src.Revision, 0); !errors.Is(err, ErrUnverifiedSource) {
		t.Fatal(err)
	}
	if got := balanceRefreshError(errors.New("write /private/config: permission denied")); got != "storage_unavailable" {
		t.Fatal(got)
	}
	if got := balanceRefreshError(ErrBusy); got != ErrBusy.Error() {
		t.Fatal(got)
	}
}

func TestBalanceIdentityForwardingOnlyAllowsExactReadQuery(t *testing.T) {
	for _, path := range []string{
		"/api/v1/admin/accounts/balance-identities",
		"/api/v1/admin/accounts/balance-identities/?ids=1",
		"/api/v1/admin/accounts/balance-identities?ids=1&ids=2",
		"/api/v1/admin/accounts/balance-identities?ids=1&token=secret",
		"/api/v1/admin/accounts/balance-identities?ids=0",
		"/api/v1/admin/accounts?ids=1",
	} {
		u, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if balanceIdentityPath(u) {
			t.Fatal("unsafe path allowed", path)
		}
	}
	u, _ := url.Parse("/api/v1/admin/accounts/balance-identities?ids=1,2")
	if !balanceIdentityPath(u) {
		t.Fatal("valid bounded identity path rejected")
	}
}

func TestRefreshRechecksIdentityAfterUpstreamResponse(t *testing.T) {
	svc, src, marker := balanceFixture(t)
	s := testServer(t, "admin")
	s.Service = svc
	var identityCalls atomic.Int32
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/me" {
			return reply(200, `{"code":0,"data":{"id":9,"role":"admin","status":"active"}}`), nil
		}
		if r.URL.Path != "/api/v1/admin/accounts/balance-identities" {
			t.Errorf("unexpected path %s", r.URL)
			return reply(404, ""), nil
		}
		currentMarker := marker
		if identityCalls.Add(1) > 1 {
			currentMarker = "v1:" + strings.Repeat("b", 64)
		}
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"accounts": []BalanceAccountIdentity{{1, true, currentMarker}}}})
		return reply(200, string(raw)), nil
	})
	if err := svc.Store.setBinding(s.SubOrigin, 1, BalanceAccountIdentity{1, true, marker}, src.ID, src.Revision, 0); err != nil {
		t.Fatal(err)
	}
	svc.Provider.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) { return reply(503, `{}`), nil })}
	res := request(s, "POST", "/api/balance/accounts/refresh", `{"account_ids":[1]}`, "fixture-admin", s.PublicOrigin)
	var out struct {
		Accounts []AccountBalanceSummary `json:"accounts"`
	}
	if res.Code != 200 {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Status != "rebind_required" || out.Accounts[0].Balance != nil || identityCalls.Load() != 2 {
		t.Fatal(out)
	}
}
