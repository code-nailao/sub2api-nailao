package portal

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// Main is the authority for whether session binding is enabled. A signed claim
// may remain present even while Main permits that session after an IP change.
func TestStudioGroupsHonorNativeIdentityDecision(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "native_allows_binding_disabled", false: "native_rejects_session"}[allow], func(t *testing.T) {
			s := testServer(t, "admin")
			s.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			s.Studio, _ = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{{GroupID: 26, Models: []string{"gpt-image-2"}, Engine: "sub2api"}}, "")
			calls := 0
			s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("X-Forwarded-For") != "203.0.113.11" || r.UserAgent() != "Browser/1" {
					t.Fatal("client identity not forwarded faithfully")
				}
				if r.URL.Path == "/api/v1/auth/me" {
					if !allow {
						return reply(401, `{"code":401,"message":"unauthorized"}`), nil
					}
					return reply(200, `{"code":0,"data":{"id":9,"role":"admin","status":"active"}}`), nil
				}
				if r.URL.Path == "/api/v1/groups/available" {
					return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
				}
				t.Fatal("unexpected native endpoint")
				return nil, nil
			})
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"bnd":"previous-network-fingerprint"}`)) + ".fixture"
			req := httptest.NewRequest("GET", "/api/studio/groups", nil)
			req.RemoteAddr = "127.0.0.1:123"
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", "Browser/1")
			req.Header.Set("X-Forwarded-For", "203.0.113.11")
			out := httptest.NewRecorder()
			s.Handler().ServeHTTP(out, req)
			if allow {
				if out.Code != 200 || !strings.Contains(out.Body.String(), `"id":26`) {
					t.Fatalf("native-valid session cannot open Studio: %d %s", out.Code, out.Body)
				}
			} else if out.Code != 401 {
				t.Fatalf("native rejection bypassed: %d", out.Code)
			}
			if calls == 0 {
				t.Fatal("Portal overrode native session policy without consulting Main")
			}
		})
	}
}
