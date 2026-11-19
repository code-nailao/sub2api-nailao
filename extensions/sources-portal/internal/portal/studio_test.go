package portal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStudioOrdinaryKeysOwnershipRevokeRestart(t *testing.T) {
	s := testServer(t, "user")
	dir := t.TempDir()
	var e error
	s.Studio, e = OpenStudio(dir, s.SubOrigin, []StudioCapability{{GroupID: 26, Models: []string{"gpt-image-2"}, Engine: "sub2api"}}, "")
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	revoked := false
	user := int64(9)
	keys := map[int64]studioKey{}
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, fmt.Sprintf(`{"code":0,"data":{"id":%d,"role":"user","status":"active"}}`, user)), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == "POST" {
				var payload map[string]any
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &payload)
				if len(payload) != 3 || payload["expires_in_days"] != float64(30) || !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "studio-") {
					t.Error("not ordinary minimal key", payload)
				}
				count++
				key := studioKey{ID: int64(count), UserID: user, GroupID: 26, Status: "active", Key: fmt.Sprintf("sk-fixture-%d", count), Name: payload["name"].(string)}
				keys[key.ID] = key
				b, _ = json.Marshal(key)
				return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
			}
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		default:
			var id int64
			_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/keys/%d", &id)
			key := keys[id]
			if revoked {
				return reply(404, ``), nil
			}
			if key.UserID != user {
				t.Error("cross-user lookup")
			}
			b, _ := json.Marshal(key)
			return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
		}
	})
	call := func(g int) int {
		r := request(s, "POST", "/api/studio/connection", fmt.Sprintf(`{"group_id":%d}`, g), "fixture-user", s.PublicOrigin)
		if r.Code == 200 && !strings.Contains(r.Body.String(), `"inputModes":["text","image"]`) {
			t.Error(r.Body.String())
		}
		return r.Code
	}
	if call(99) != 403 {
		t.Fatal("group bypass")
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if call(26) != 200 {
				t.Error("connect")
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatal("duplicate keys", count)
	}
	s.Studio, e = OpenStudio(dir, s.SubOrigin, s.Studio.Capabilities, "")
	if e != nil {
		t.Fatal(e)
	}
	if call(26) != 200 || count != 1 {
		t.Fatal("restart")
	}
	revoked = true
	if call(26) != 403 || count != 1 {
		t.Fatal("revoked key recreated")
	}
	revoked = false
	user = 10
	if call(26) != 200 || count != 2 {
		t.Fatal("user isolation")
	}
	disk, _ := os.ReadFile(filepath.Join(dir, "studio-bindings.json"))
	if strings.Contains(string(disk), "sk-fixture") {
		t.Fatal("API key persisted")
	}
}
func TestIdentityBindingForwardingAndNativeEnforcement(t *testing.T) {
	s := testServer(t, "admin")
	s.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	calls := 0
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-Forwarded-For") != "203.0.113.10" || r.UserAgent() != "Browser/1" {
			return reply(401, `{"code":401}`), nil
		}
		return reply(200, `{"code":0,"data":{"id":9,"role":"admin","status":"active"}}`), nil
	})
	sum := sha256.Sum256([]byte("203.0.113.10\nBrowser/1"))
	b := base64.RawURLEncoding.EncodeToString([]byte(`{"bnd":"` + hex.EncodeToString(sum[:16]) + `"}`))
	token := "header." + b + ".untrusted-payload-is-not-auth"
	r := httptest.NewRequest("GET", "/api/balance/sources", nil)
	r.RemoteAddr = "127.0.0.1:123"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("User-Agent", "Browser/1")
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	if _, e := s.identity(r); e != nil || calls != 1 {
		t.Fatal(e)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.11")
	if _, e := s.identity(r); e == nil || calls != 2 {
		t.Fatal("native fingerprint rejection was not enforced")
	}
	r.RemoteAddr = "203.0.113.22:22"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	if _, e := s.identity(r); e == nil || calls != 3 {
		t.Fatal("untrusted XFF accepted")
	}
}
func TestHomeHTTPAuthAndDraftPrivacy(t *testing.T) {
	s := testServer(t, "admin")
	var e error
	s.Content, e = OpenContentStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s.HomeFiles = webFiles
	baseline := request(s, "GET", "/public/home", "", "", "").Body.String()
	c := s.Content.Read().Draft
	c.Brand.HeroTitle = "private draft"
	body, _ := json.Marshal(map[string]any{"revision": 1, "config": c})
	if r := request(s, "POST", "/api/home/draft", string(body), "", "https://evil.test"); r.Code != 403 {
		t.Fatal(r.Code)
	}
	r := request(s, "POST", "/api/home/draft", string(body), "fixture-admin", s.PublicOrigin)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if request(s, "GET", "/public/home", "", "", "").Body.String() != baseline {
		t.Fatal("draft leaked")
	}
	if r = request(s, "POST", "/api/home/publish", `{"revision":2}`, "fixture-admin", s.PublicOrigin); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if !bytes.Contains(request(s, "GET", "/public/home", "", "", "").Body.Bytes(), []byte("private draft")) {
		t.Fatal("not published")
	}
}

func TestStudioAmbiguousCreationIsReconciledNotRepeated(t *testing.T) {
	s := testServer(t, "user")
	s.Studio, _ = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{{GroupID: 26, Models: []string{"gpt-image-2"}, Engine: "sub2api"}}, "")
	attempts := 0
	var created studioKey
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == "POST" {
				attempts++
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				created = studioKey{ID: 8, UserID: 9, GroupID: 26, Name: payload["name"].(string), Status: "active", Key: "sk-fixture"}
				return nil, io.ErrUnexpectedEOF
			}
			list := []studioKey{}
			if attempts > 0 {
				list = append(list, created)
			}
			b, _ := json.Marshal(list)
			return reply(200, `{"code":0,"data":{"items":`+string(b)+`}}`), nil
		case "/api/v1/keys/8/name":
			if r.Method != http.MethodPatch {
				t.Fatal(r.Method)
			}
			return reply(404, ``), nil // Older Main safely defers cosmetic renaming.
		}
		t.Fatal(r.URL)
		return nil, io.ErrUnexpectedEOF
	})
	if r := request(s, "POST", "/api/studio/connection", `{"group_id":26}`, "user", s.PublicOrigin); r.Code != 503 {
		t.Fatal(r.Code)
	}
	if r := request(s, "POST", "/api/studio/connection", `{"group_id":26}`, "user", s.PublicOrigin); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if attempts != 1 {
		t.Fatal("repeated POST", attempts)
	}
}
func TestStudioLockCancellationAndIndependentUsers(t *testing.T) {
	s, _ := OpenStudio(t.TempDir(), "https://main.example", nil, "")
	unlock, e := s.lock(context.Background(), "9:26")
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	other, e := s.lock(context.Background(), "10:26")
	if e != nil {
		t.Fatal(e)
	}
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = s.lock(ctx, "9:26"); e != context.Canceled {
		t.Fatal(e)
	}
}

func TestMainClientCannotCallBalanceAdminOrArbitraryEndpoints(t *testing.T) {
	s := testServer(t, "admin")
	calls := 0
	s.AuthClient.Transport = roundTrip(func(*http.Request) (*http.Response, error) { calls++; return reply(500, ""), nil })
	for _, test := range []struct{ method, path string }{{"POST", "/api/v1/admin/users/9/balance"}, {"GET", "https://elsewhere.example/api/v1/auth/me"}, {"GET", "//elsewhere.example/api/v1/auth/me"}, {"DELETE", "/api/v1/keys/1"}, {"PUT", "/api/v1/admin/settings"}} {
		if e := s.mainAPI(httptest.NewRequest("GET", "/", nil), test.method, test.path, nil, "", nil); e != ErrCredential {
			t.Fatal(test, e)
		}
	}
	if calls != 0 {
		t.Fatal("escaped operation reached transport")
	}
}

// The engine value reaches the separately deployed Studio client, so with a
// configured legacy name it must be exactly that value, whichever spelling the
// registry uses. The legacy name here is deliberately fake.
func TestStudioMediaEngineGeminiIndependentCapabilities(t *testing.T) {
	for _, tc := range []struct{ legacy, configured, emitted string }{
		{"", "media", "media"},
		{"legacyname", "legacyname", "legacyname"},
		{"legacyname", "media", "legacyname"},
	} {
		t.Run(tc.legacy+"/"+tc.configured, func(t *testing.T) { testStudioMediaEngineGemini(t, tc.legacy, tc.configured, tc.emitted) })
	}
}

func TestStudioMediaEngineLegacyNameIsRequiredAndValidated(t *testing.T) {
	if _, err := OpenStudio(t.TempDir(), "https://main.example", []StudioCapability{{GroupID: 81, Engine: "legacyname", Models: []string{"gpt-image-2"}}}, ""); err == nil {
		t.Fatal("a legacy engine value was accepted without the legacy name")
	}
	for _, invalid := range []string{"Legacyname", "legacy-name", "x", "sub2api", strings.Repeat("a", 33)} {
		_, err := OpenStudio(t.TempDir(), "https://main.example", nil, invalid)
		if err == nil || strings.Contains(err.Error(), invalid) {
			t.Fatalf("invalid legacy name: %v", err)
		}
	}
	caps := []StudioCapability{{GroupID: 81, Engine: "media", Models: []string{"gpt-image-2"}}}
	if _, err := OpenStudio(t.TempDir(), "https://main.example", caps, "legacyname"); err != nil || caps[0].Engine != "media" {
		t.Fatal("the caller's configuration was rewritten", err)
	}
}

func testStudioMediaEngineGemini(t *testing.T, legacy, configured, emitted string) {
	s := testServer(t, "user")
	var err error
	s.Studio, err = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{{GroupID: 81, Engine: configured, Models: []string{"gemini-3-pro-image"}}}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":81,"name":"Gemini media engine","platform":"gemini","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == "POST" {
				return reply(200, `{"code":0,"data":{"id":42,"user_id":9,"group_id":81,"status":"active","key":"fixture-user-key"}}`), nil
			}
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		}
		return reply(404, `{}`), nil
	})
	response := request(s, "POST", "/api/studio/connection", `{"group_id":81}`, "fixture-user", s.PublicOrigin)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, value := range []string{`"engine":"` + emitted + `"`, `"protocol":"openai_images"`, `"key":"mask"`, `"max":4`, `"editPath":"/v1/images/edits/async"`} {
		if !strings.Contains(response.Body.String(), value) {
			t.Fatal("missing capability", value, response.Body.String())
		}
	}
}

func TestStudioGrokUsesNativeImageCapability(t *testing.T) {
	s := testServer(t, "user")
	var err error
	s.Studio, err = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{{GroupID: 162, Models: []string{"grok-imagine-image"}, Engine: "sub2api"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":162,"name":"Grok","platform":"grok","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == "POST" {
				return reply(200, `{"code":0,"data":{"id":1,"user_id":9,"group_id":162,"status":"active","key":"sk-fixture-grok"}}`), nil
			}
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		default:
			t.Fatalf("unexpected native API: %s", r.URL.Path)
			return nil, nil
		}
	})
	result := request(s, "POST", "/api/studio/connection", `{"group_id":162}`, "fixture-user", s.PublicOrigin)
	if result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	var value struct {
		Capabilities []struct {
			Protocol string `json:"protocol"`
			Models   []struct {
				Parameters []struct {
					Key, Type string
					Options   []struct{ Value string }
				}
			} `json:"models"`
			Task struct{ Engine, CreatePath string } `json:"task"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	capability := value.Capabilities[0]
	if capability.Protocol != "grok_image" || capability.Task.Engine != "sub2api" || capability.Task.CreatePath != "/v1/images/generations/async" {
		t.Fatalf("incorrect native Grok contract: %+v", capability)
	}
	parameters := map[string][]string{}
	for _, p := range capability.Models[0].Parameters {
		for _, option := range p.Options {
			parameters[p.Key] = append(parameters[p.Key], option.Value)
		}
	}
	if strings.Join(parameters["resolution"], ",") != "1k,2k" || !strings.Contains(strings.Join(parameters["aspect_ratio"], ","), "3:2") {
		t.Fatalf("native geometry missing: %+v", parameters)
	}
	if _, ok := parameters["quality"]; ok {
		t.Fatal("OpenAI quality must not leak into Grok controls")
	}
}
