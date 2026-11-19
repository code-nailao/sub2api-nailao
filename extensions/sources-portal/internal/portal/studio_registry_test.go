package portal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registryFixture is a Main with one registered group (26), one group nobody
// registered (27), a media engine group (30) and two groups users cannot be
// offered yet (28: images off, 29: unsupported platform).
func registryFixture(t *testing.T, dir string) (*Server, *string) {
	t.Helper()
	s := testServer(t, "user")
	var e error
	s.Studio, e = OpenStudio(dir, s.SubOrigin, []StudioCapability{
		{GroupID: 26, Engine: "sub2api", Models: []string{"gpt-image-2"}},
		{GroupID: 30, Engine: "media", Models: []string{"gpt-image-2"}},
	}, "")
	if e != nil {
		t.Fatal(e)
	}
	role := "user"
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"`+role+`","status":"active"}}`), nil
		case r.URL.Path == "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true},{"id":27,"name":"Grok","platform":"grok","status":"active","allow_image_generation":true},{"id":30,"name":"Media","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
		case r.URL.Path == mainGroupListPath:
			if role != "admin" {
				return reply(403, `{"code":403}`), nil
			}
			return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true},{"id":27,"name":"Grok","platform":"grok","status":"active","allow_image_generation":true},{"id":28,"name":"Text","platform":"openai","status":"active","allow_image_generation":false},{"id":29,"name":"Claude","platform":"anthropic","status":"active","allow_image_generation":true},{"id":30,"name":"Media","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
		case r.URL.Path == "/api/v1/admin/groups/27/model-allowlist-candidates":
			return reply(200, `{"code":0,"data":{"models":["grok-imagine-image","bad model","grok-4"]}}`), nil
		case r.URL.Path == "/api/v1/keys" && r.Method == http.MethodPost:
			return reply(200, `{"code":0,"data":{"id":1,"user_id":9,"group_id":27,"key":"sk-fixture","status":"active"}}`), nil
		case r.URL.Path == "/api/v1/keys":
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		case r.URL.Path == "/api/v1/keys/1":
			return reply(200, `{"code":0,"data":{"id":1,"user_id":9,"group_id":27,"key":"sk-fixture","status":"active"}}`), nil
		}
		t.Errorf("unexpected Main request: %s %s", r.Method, r.URL.Path)
		return reply(404, `{}`), nil
	})
	return s, &role
}

func registryCall(s *Server, method, path, body string) *httptest.ResponseRecorder {
	return request(s, method, path, body, "fixture", s.PublicOrigin)
}

func TestStudioRegistryAddsGroupsWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	s, role := registryFixture(t, dir)
	visible := func(id int) bool {
		groups := registryCall(s, "GET", "/api/studio/groups", "")
		if groups.Code != 200 {
			t.Fatal(groups.Code, groups.Body.String())
		}
		return strings.Contains(groups.Body.String(), fmt.Sprintf(`"id":%d,`, id))
	}
	if !visible(26) || visible(27) {
		t.Fatal("registry file is not the starting point")
	}
	if strings.Contains(registryCall(s, "GET", "/api/studio/groups", "").Body.String(), `"can_manage":true`) {
		t.Fatal("ordinary user offered group settings")
	}
	for _, call := range []struct{ method, path, body string }{
		{"GET", "/api/studio/admin/registry", ""},
		{"PUT", "/api/studio/admin/registry", `{"revision":0,"groups":[{"group_id":27,"models":["grok-imagine-image"],"enabled":true}]}`},
		{"GET", "/api/studio/admin/groups/27/models", ""},
	} {
		if res := registryCall(s, call.method, call.path, call.body); res.Code != 403 {
			t.Fatalf("ordinary user reached %s %s: %d", call.method, call.path, res.Code)
		}
	}
	if visible(27) {
		t.Fatal("ordinary user changed the registry")
	}

	*role = "admin"
	view := registryCall(s, "GET", "/api/studio/admin/registry", "")
	var state struct {
		Revision int64
		Writable bool
		Groups   []studioRegistryRow
	}
	if view.Code != 200 || json.Unmarshal(view.Body.Bytes(), &state) != nil || state.Revision != 0 || !state.Writable || len(state.Groups) != 5 {
		t.Fatal(view.Code, view.Body.String())
	}
	rows := map[int64]studioRegistryRow{}
	for _, row := range state.Groups {
		rows[row.GroupID] = row
	}
	if rows[26].Source != "file" || !rows[26].Enabled || rows[27].Source != "none" || rows[27].Enabled || !rows[30].Locked ||
		strings.Join(rows[28].Blockers, ",") != "image_generation_disabled" || strings.Join(rows[29].Blockers, ",") != "unsupported_platform" {
		t.Fatalf("registry view: %+v", rows)
	}
	// The engine name of the media group is deployment configuration.
	if strings.Contains(view.Body.String(), "engine") {
		t.Fatal("engine exposed to the settings dialog")
	}
	models := registryCall(s, "GET", "/api/studio/admin/groups/27/models", "")
	if models.Code != 200 || models.Body.String() != `{"models":["grok-imagine-image","grok-4"]}`+"\n" {
		t.Fatal(models.Code, models.Body.String())
	}

	for name, body := range map[string]string{
		"engine from request": `{"revision":0,"groups":[{"group_id":27,"engine":"media","models":["m"],"enabled":true}]}`,
		"no models":           `{"revision":0,"groups":[{"group_id":27,"models":[],"enabled":true}]}`,
		"malformed model":     `{"revision":0,"groups":[{"group_id":27,"models":["a b"],"enabled":true}]}`,
		"reserved group":      fmt.Sprintf(`{"revision":0,"groups":[{"group_id":%d,"models":["m"],"enabled":true}]}`, studioCustomGroupID),
		"duplicate group":     `{"revision":0,"groups":[{"group_id":27,"models":["m"],"enabled":true},{"group_id":27,"models":["m"],"enabled":true}]}`,
	} {
		if res := registryCall(s, "PUT", "/api/studio/admin/registry", body); res.Code != 400 {
			t.Fatalf("%s accepted: %d %s", name, res.Code, res.Body.String())
		}
	}
	if res := registryCall(s, "PUT", "/api/studio/admin/registry", `{"revision":0,"groups":[{"group_id":30,"models":["gpt-image-2"],"enabled":false}]}`); res.Code != 409 || !strings.Contains(res.Body.String(), ErrStudioServerManaged.Error()) {
		t.Fatal("media engine group changed from the browser", res.Code, res.Body.String())
	}

	saved := registryCall(s, "PUT", "/api/studio/admin/registry", `{"revision":0,"groups":[{"group_id":27,"models":[" grok-imagine-image ","grok-imagine-image"],"enabled":true}]}`)
	if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"revision":1`) || !strings.Contains(saved.Body.String(), `"source":"managed"`) {
		t.Fatal(saved.Code, saved.Body.String())
	}
	if res := registryCall(s, "PUT", "/api/studio/admin/registry", `{"revision":0,"groups":[]}`); res.Code != 409 {
		t.Fatal("stale revision overwrote a newer save", res.Code)
	}

	*role = "user"
	if !visible(27) || !visible(26) {
		t.Fatal("saved group is not offered to users at once")
	}
	connection := registryCall(s, "POST", "/api/studio/connection", `{"group_id":27}`)
	if connection.Code != 200 || !strings.Contains(connection.Body.String(), `"id":"grok-imagine-image"`) || !strings.Contains(connection.Body.String(), `"protocol":"grok_image"`) || strings.Count(connection.Body.String(), `"isDefault"`) != 1 {
		t.Fatal(connection.Code, connection.Body.String())
	}

	// An override replaces a native file entry; disabled hides it.
	*role = "admin"
	if res := registryCall(s, "PUT", "/api/studio/admin/registry", `{"revision":1,"groups":[{"group_id":27,"models":["grok-imagine-image"],"enabled":true},{"group_id":26,"models":["gpt-image-2"],"enabled":false}]}`); res.Code != 200 || !strings.Contains(res.Body.String(), `"source":"override"`) {
		t.Fatal(res.Code, res.Body.String())
	}
	*role = "user"
	if visible(26) || !visible(27) || !visible(30) {
		t.Fatal("override not applied")
	}
	if res := registryCall(s, "POST", "/api/studio/connection", `{"group_id":26}`); res.Code != 403 {
		t.Fatal("hidden group still authorizes", res.Code)
	}

	// The same state after a restart, and nothing but group IDs and models on disk.
	var e error
	if s.Studio, e = OpenStudio(dir, s.SubOrigin, s.Studio.Capabilities, ""); e != nil {
		t.Fatal(e)
	}
	if visible(26) || !visible(27) {
		t.Fatal("registry lost on restart")
	}
	disk, _ := os.ReadFile(filepath.Join(dir, "studio-registry.json"))
	if !strings.Contains(string(disk), `"revision": 2`) || strings.Contains(string(disk), "sk-") {
		t.Fatal(string(disk))
	}
}

func TestStudioRegistryFileOwnsMediaEngineGroups(t *testing.T) {
	file := []StudioCapability{{GroupID: 30, Engine: "media", Models: []string{"gpt-image-2"}}, {GroupID: 26, Engine: "sub2api", Models: []string{"gpt-image-2"}}}
	managed := []StudioManagedGroup{{GroupID: 30, Models: []string{"other"}, Enabled: false}, {GroupID: 26, Models: []string{"a", "b"}, Enabled: true}, {GroupID: 41, Models: []string{"c"}, Enabled: false}, {GroupID: 42, Models: []string{"d"}, Enabled: true}}
	got := mergeStudioCapabilities(file, managed)
	b, _ := json.Marshal(got)
	if string(b) != `[{"group_id":30,"models":["gpt-image-2"],"engine":"media"},{"group_id":26,"models":["a","b"],"engine":"sub2api"},{"group_id":42,"models":["d"],"engine":"sub2api"}]` {
		t.Fatal(string(b))
	}
}

func TestStudioRegistryRejectsDamagedOrReservedState(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       `{`,
		"no revision":    `{"groups":[]}`,
		"bad model":      `{"revision":1,"groups":[{"group_id":27,"models":["a b"],"enabled":true}]}`,
		"reserved group": fmt.Sprintf(`{"revision":1,"groups":[{"group_id":%d,"models":["m"],"enabled":true}]}`, studioReservedGroupID),
	} {
		dir := t.TempDir()
		if e := os.WriteFile(filepath.Join(dir, "studio-registry.json"), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := OpenStudio(dir, "https://main.example", nil, ""); e == nil {
			t.Fatalf("%s: started on a damaged registry", name)
		}
	}
	if _, e := OpenStudio(t.TempDir(), "https://main.example", []StudioCapability{{GroupID: studioCustomGroupID, Engine: "sub2api", Models: []string{"m"}}}, ""); e == nil {
		t.Fatal("registry file used a reserved group ID")
	}
	// An empty file is a valid start: every native group can be managed in the browser.
	if s, e := OpenStudio(t.TempDir(), "https://main.example", nil, ""); e != nil || len(s.capabilities()) != 0 {
		t.Fatal(e)
	}
}

func TestStudioRegistryReadsAreTheOnlyNewAdminPaths(t *testing.T) {
	s := testServer(t, "admin")
	reached := []string{}
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.Method+" "+r.URL.RequestURI())
		return reply(200, `{"code":0,"data":[]}`), nil
	})
	source := httptest.NewRequest("GET", "/", nil)
	for _, test := range []struct{ method, path string }{
		{"GET", mainGroupListPath + "?include_inactive=true"}, {"POST", mainGroupListPath}, {"PUT", "/api/v1/admin/groups/27"},
		{"GET", "/api/v1/admin/groups/27"}, {"DELETE", "/api/v1/admin/groups/27"}, {"GET", "/api/v1/admin/groups/27/api-keys"},
		{"GET", "/api/v1/admin/groups/0/model-allowlist-candidates"}, {"GET", "/api/v1/admin/groups/27/model-allowlist-candidates?platform=x"},
		{"GET", "/api/v1/admin/groups/27%2F..%2F..%2Fusers/model-allowlist-candidates"},
	} {
		if e := s.mainAPI(source, test.method, test.path, nil, "", nil); e != ErrCredential {
			t.Fatal(test, e)
		}
	}
	if len(reached) != 0 {
		t.Fatal("refused path reached Main", reached)
	}
	for _, path := range []string{mainGroupListPath, "/api/v1/admin/groups/27/model-allowlist-candidates"} {
		if e := s.mainAPI(source, "GET", path, nil, "", nil); e != nil {
			t.Fatal(path, e)
		}
	}
	if strings.Join(reached, "|") != "GET "+mainGroupListPath+"|GET /api/v1/admin/groups/27/model-allowlist-candidates" {
		t.Fatal(reached)
	}
}
