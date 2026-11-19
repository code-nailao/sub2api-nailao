package processrole

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPManagementUsesUniqueBackgroundOwner(t *testing.T) {
	cases := []struct {
		method string
		path   string
		owner  bool
	}{
		{"POST", "/api/v1/mcp", true},
		{"GET", "/api/v1/mcp", true},
		{"DELETE", "/api/v1/mcp", true},
		{"POST", "/api/v1/mcp/oauth/register", true},
		{"POST", "/api/v1/mcp/oauth/token", true},
		{"POST", "/api/v1/mcp/oauth/revoke", true},
		{"GET", "/.well-known/oauth-protected-resource/api/v1/mcp", true},
		{"GET", "/.well-known/oauth-protected-resource", true},
		{"GET", "/.well-known/oauth-authorization-server", true},
		{"GET", "/api/v1/user/mcp/config", false},
		{"GET", "/api/v1/user/mcp/grants", false},
		{"DELETE", "/api/v1/user/mcp/grants/1", false},
		{"POST", "/api/v1/user/mcp/authorize", false},
		{"GET", "/settings/mcp", false},
		{"GET", "/oauth/mcp/authorize", false},
		{"POST", "/api/v1/mcp-other", false},
		{"GET", "/.well-known/oauth-protected-resource-other", false},
	}
	for _, tc := range cases {
		for _, role := range []Role{Serving, Background, Standalone} {
			t.Run(string(role)+tc.method+tc.path, func(t *testing.T) {
				r := New(Config{Role: role})
				called := false
				h := r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					called = true
					if r.Status().ActiveHTTP != 1 {
						t.Fatal("management request not tracked for drain")
					}
					w.WriteHeader(http.StatusUnauthorized) // Native auth still runs after the role boundary.
				}))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
				allowed := role == Standalone || (role == Background) == tc.owner
				want := http.StatusServiceUnavailable
				if allowed {
					want = http.StatusUnauthorized
				}
				if w.Code != want || called != allowed || r.Status().ActiveHTTP != 0 {
					t.Fatalf("status=%d called=%v active=%d; want status=%d called=%v", w.Code, called, r.Status().ActiveHTTP, want, allowed)
				}
			})
		}
	}
}
