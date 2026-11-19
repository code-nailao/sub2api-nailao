package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T, role string) *Server {
	svc := newTestService(t)
	return &Server{Service: svc, SubOrigin: "https://main.example", PublicOrigin: "https://portal.example", AuthClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://main.example/api/v1/auth/me" || r.Method != "GET" {
			t.Fatalf("auth target %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-admin" {
			return reply(401, ""), nil
		}
		return reply(200, `{"code":0,"data":{"id":9,"role":"`+role+`","status":"active"}}`), nil
	})}}
}
func request(s *Server, method, path, body, token, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	return out
}
func TestAdminIdentityAndOriginAreServerVerified(t *testing.T) {
	for _, tc := range []struct {
		role, token, origin string
		status              int
	}{{"admin", "", "", 401}, {"user", "fixture-admin", "", 403}, {"admin", "wrong", "", 401}, {"admin", "fixture-admin", "https://evil.example", 403}, {"admin", "fixture-admin", "https://portal.example", 200}} {
		s := testServer(t, tc.role)
		res := request(s, "GET", "/api/balance/sources?role=admin&user_id=9", "", tc.token, tc.origin)
		if res.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, res.Code, res.Body.String())
		}
	}
}
func TestSourceCRUDAndRefreshNeverReturnsPersonalToken(t *testing.T) {
	s := testServer(t, "admin")
	b, _ := json.Marshal(input())
	res := request(s, "POST", "/api/balance/sources", string(b), "fixture-admin", s.PublicOrigin)
	if res.Code != 200 {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	var src Source
	_ = json.Unmarshal(res.Body.Bytes(), &src)
	if src.UpdatedBy != 9 {
		t.Fatal("actor not verified")
	}
	for _, path := range []string{"/api/balance/sources", "/api/balance/sources/" + src.ID + "/refresh"} {
		method := "GET"
		if strings.HasSuffix(path, "refresh") {
			method = "POST"
		}
		out := request(s, method, path, "", "fixture-admin", s.PublicOrigin)
		if out.Code != 200 || strings.Contains(out.Body.String(), "personal-fixture") || strings.Contains(out.Body.String(), "encrypted_token") {
			t.Fatalf("%s %d %s", path, out.Code, out.Body)
		}
	}
	res = request(s, "DELETE", "/api/balance/sources/"+src.ID, `{"revision":0}`, "fixture-admin", s.PublicOrigin)
	if res.Code != 409 {
		t.Fatal(res.Code)
	}
	res = request(s, "DELETE", "/api/balance/sources/"+src.ID, `{"revision":1}`, "fixture-admin", s.PublicOrigin)
	if res.Code != 204 || len(s.Service.List()) != 0 {
		t.Fatal(res.Code)
	}
}
func TestStaticPageHeadersAndUnknownJSONFields(t *testing.T) {
	s := testServer(t, "admin")
	res := request(s, "GET", "/admin/balances?token=fixture-admin", "", "", "")
	if res.Code != 303 || res.Header().Get("Location") != s.SubOrigin+"/admin/accounts" || res.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(res.Header().Get("Content-Security-Policy"), "frame-ancestors https://main.example") {
		t.Fatalf("%d %+v", res.Code, res.Header())
	}
	if strings.Contains(res.Body.String(), "fixture-admin") {
		t.Fatal("query token reflected")
	}
	res = request(s, "POST", "/api/balance/sources", `{"name":"x","role":"admin"}`, "fixture-admin", s.PublicOrigin)
	if res.Code != 400 {
		t.Fatal(res.Code)
	}
	res = request(s, "GET", "/sources.json", "", "", "")
	if res.Code != 404 {
		t.Fatal("store exposed")
	}
}
func TestCancelledWorkerStops(t *testing.T) {
	svc := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.Run(ctx)
}

func TestIdentityOutageIs503NotExpiredLogin(t *testing.T) {
	s := testServer(t, "admin")
	s.AuthClient.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return reply(503, "private upstream details"), nil })
	out := request(s, "GET", "/api/balance/sources", "", "fixture-admin", s.PublicOrigin)
	if out.Code != 503 || !strings.Contains(out.Body.String(), "main_identity_unavailable") || strings.Contains(out.Body.String(), "private") {
		t.Fatalf("%d %s", out.Code, out.Body)
	}
}
