package portal

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStudioConditionalNameAfterDurableBinding(t *testing.T) {
	for _, scenario := range []string{"success", "conflict", "unsupported", "lost-response", "custom-name", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t, "user")
			dir := t.TempDir()
			var err error
			s.Studio, err = OpenStudio(dir, s.SubOrigin, []StudioCapability{{GroupID: 26, Models: []string{"gpt-image-2"}, Engine: "sub2api"}}, "")
			if err != nil {
				t.Fatal(err)
			}
			binding := StudioBinding{Attempted: true, UserID: 9, GroupID: 26, KeyID: 7, Operation: "operation-fixture", CreatedAt: time.Now()}
			if err := s.Studio.save("9:26", binding); err != nil {
				t.Fatal(err)
			}
			key := studioKey{ID: 7, UserID: 9, GroupID: 26, Key: "private-fixture-key", Name: "图片工作台 · operation-fixture", Status: "active"}
			if scenario == "custom-name" {
				key.Name = "我自己的名称"
			}
			if scenario == "revoked" {
				key.Status = "inactive"
			}
			patches := 0
			s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Path == "/api/v1/auth/me":
					return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active","username":"测试用户","balance":0}}`), nil
				case r.URL.Path == "/api/v1/groups/available":
					return reply(200, `{"code":0,"data":[{"id":26,"name":"Images & edit","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
				case r.URL.Path == "/api/v1/keys/7" && r.Method == "GET":
					b, _ := json.Marshal(key)
					return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
				case r.URL.Path == "/api/v1/keys/7/name" && r.Method == "PATCH":
					patches++
					b, _ := os.ReadFile(filepath.Join(dir, "studio-bindings.json"))
					var saved map[string]StudioBinding
					if json.Unmarshal(b, &saved) != nil || saved["9:26"].KeyID != 7 {
						t.Fatal("rename before durable binding")
					}
					var in map[string]any
					if json.NewDecoder(r.Body).Decode(&in) != nil || len(in) != 3 || in["name"] != "生图 · Images & edit" || in["expected_name"] != "图片工作台 · operation-fixture" || in["expected_group_id"] != float64(26) {
						t.Fatalf("unexpected rename fields: %#v", in)
					}
					if scenario == "unsupported" {
						return reply(404, ""), nil
					}
					if scenario == "conflict" {
						key.Name = "用户并发改名"
						return reply(409, ""), nil
					}
					key.Name = html.EscapeString(in["name"].(string))
					if scenario == "lost-response" {
						return nil, io.EOF
					}
					return reply(200, `{"code":0,"data":{"id":7,"name":"生图 · Images &amp; edit"}}`), nil
				default:
					t.Fatalf("unexpected request / unconditional fallback: %s %s", r.Method, r.URL.Path)
					return nil, fmt.Errorf("unexpected request")
				}
			})
			for i := 0; i < 2; i++ {
				r := request(s, "POST", "/api/studio/connection", `{"group_id":26}`, "fixture-user", s.PublicOrigin)
				want := 200
				if scenario == "revoked" {
					want = 403
				}
				if r.Code != want {
					t.Fatalf("status %d: %s", r.Code, r.Body.String())
				}
			}
			want := 1
			if scenario == "custom-name" || scenario == "revoked" {
				want = 0
			}
			if scenario == "unsupported" {
				want = 2
			}
			if patches != want {
				t.Fatalf("patch count %d, want %d", patches, want)
			}
		})
	}
}

func TestStudioKeyDisplayNameFitsEscapedUTF8Limit(t *testing.T) {
	for _, group := range []string{"Grok Ultra", strings.Repeat("中文", 80), strings.Repeat("&<\"'", 80)} {
		got := studioKeyDisplayName(group)
		if !utf8.ValidString(got) || len(html.EscapeString(got)) > 100 || !strings.HasPrefix(got, "生图 ·") {
			t.Fatalf("invalid display name %q", got)
		}
	}
}

func TestStudioGroupsMinimalViewer(t *testing.T) {
	for _, balance := range []string{"", `,"balance":0`, `,"balance":-1.25`} {
		s := testServer(t, "user")
		s.Studio, _ = OpenStudio(t.TempDir(), s.SubOrigin, nil, "")
		s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/v1/auth/me" {
				return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active","username":"","email":"name@private.example"`+balance+`}}`), nil
			}
			return reply(200, `{"code":0,"data":[]}`), nil
		})
		r := request(s, "GET", "/api/studio/groups", "", "fixture-user", s.PublicOrigin)
		if r.Code != 200 || strings.Contains(r.Body.String(), "private.example") || strings.Contains(r.Body.String(), `"email"`) {
			t.Fatal(r.Code, r.Body.String())
		}
		var data struct {
			Viewer map[string]any `json:"viewer"`
		}
		if json.Unmarshal(r.Body.Bytes(), &data) != nil || len(data.Viewer) != 2 || data.Viewer["displayName"] != "" {
			t.Fatal(r.Body.String())
		}
		if balance == "" && data.Viewer["balance"] != nil {
			t.Fatal("missing balance fabricated")
		}
		if balance != "" && data.Viewer["balance"] == nil {
			t.Fatal("real balance lost")
		}
	}
}

func TestStudioNameRouteAllowlist(t *testing.T) {
	s := testServer(t, "user")
	calls := 0
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) { calls++; return reply(200, `{"code":0,"data":{}}`), nil })
	r := httptest.NewRequest("GET", "/", nil)
	for _, path := range []string{"/api/v1/keys/7/name?x=1", "/api/v1/keys/7/%6eame", "/api/v1/keys/0/name", "/api/v1/keys/7/name/extra", "/api/v1/keys/7", "/api/v1/admin/settings", "https://other.example/api/v1/keys/7/name"} {
		if err := s.mainAPI(r, "PATCH", path, nil, "", nil); err == nil {
			t.Fatal("unsafe path allowed", path)
		}
	}
	if calls != 0 {
		t.Fatal("unsafe upstream request")
	}
	if err := s.mainAPI(r, "PATCH", "/api/v1/keys/7/name", nil, "", nil); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
