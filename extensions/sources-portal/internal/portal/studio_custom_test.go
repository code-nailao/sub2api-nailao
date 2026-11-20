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

type customFixture struct {
	s       *Server
	user    int64
	role    string
	revoked bool
	created int
	models  []string
	// moved replaces a key's reply, as when an administrator moves the key to
	// another group after the user saved a connection with it.
	moved map[string]string
}

// User 9 owns key 11 (OpenAI group), 12 (Gemini group), 13 (expired), 14 (a
// media engine group) and 16 (an Antigravity group). Key 15 belongs to user 10.
func newCustomFixture(t *testing.T, dir string) *customFixture {
	t.Helper()
	f := &customFixture{s: testServer(t, "user"), user: 9, role: "user"}
	var e error
	f.s.Studio, e = OpenStudio(dir, f.s.SubOrigin, []StudioCapability{{GroupID: 30, Engine: "media", Models: []string{"gpt-image-2"}}}, "")
	if e != nil {
		t.Fatal(e)
	}
	f.s.CanvasURL = "https://canvas.example.com/canvas"
	keys := map[string]string{
		"11": `{"id":11,"user_id":9,"group_id":40,"key":"sk-own-openai-0011","name":"mine","status":"active","group":{"name":"New images","platform":"openai"}}`,
		"12": `{"id":12,"user_id":9,"group_id":41,"key":"sk-own-gemini-0012","name":"gem","status":"active","group":{"name":"Gemini","platform":"gemini"}}`,
		"13": `{"id":13,"user_id":9,"group_id":40,"key":"sk-own-expired-13","name":"old","status":"active","expires_at":"2020-01-01T00:00:00Z"}`,
		"14": `{"id":14,"user_id":9,"group_id":30,"key":"sk-own-media-0014","name":"media","status":"active","group":{"name":"Media","platform":"openai"}}`,
		"15": `{"id":15,"user_id":10,"group_id":40,"key":"sk-other-user-0015","name":"theirs","status":"active"}`,
		"16": `{"id":16,"user_id":9,"group_id":42,"key":"sk-own-antigr-0016","name":"anti","status":"active","group":{"name":"Antigravity","platform":"antigravity"}}`,
	}
	f.s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			return reply(200, fmt.Sprintf(`{"code":0,"data":{"id":%d,"role":"%s","status":"active"}}`, f.user, f.role)), nil
		case r.URL.Path == "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[]}`), nil
		case r.URL.Path == mainGroupListPath:
			return reply(200, `{"code":0,"data":[]}`), nil
		case r.URL.Path == "/api/v1/keys" && r.Method == http.MethodGet:
			return reply(200, `{"code":0,"data":{"items":[`+keys["11"]+`,`+keys["12"]+`,`+keys["13"]+`,`+keys["14"]+`,`+keys["16"]+`]}}`), nil
		case r.URL.Path == "/api/v1/keys":
			f.created++
			return reply(500, `{}`), nil
		case strings.HasPrefix(r.URL.Path, "/api/v1/keys/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/keys/")
			key, ok := keys[id]
			if moved, isMoved := f.moved[id]; isMoved {
				key = moved
			}
			if !ok || f.revoked {
				return reply(404, `{}`), nil
			}
			return reply(200, `{"code":0,"data":`+key+`}`), nil
		case strings.HasSuffix(r.URL.Path, "/models"):
			f.models = append(f.models, r.URL.Path+" "+r.Header.Get("Authorization"))
			if r.Method != http.MethodGet || r.URL.RawQuery != "" {
				t.Errorf("model listing changed: %s %s", r.Method, r.URL.RequestURI())
			}
			if strings.HasSuffix(r.URL.Path, "v1beta/models") {
				return reply(200, `{"models":[{"name":"models/gemini-3-pro-image-preview"},{"name":"models/gemini-3-pro"},{"name":"models/bad id"}]}`), nil
			}
			return reply(200, `{"object":"list","data":[{"id":"gpt-image-2"},{"id":"gpt-5"},{"id":"bad id"},{"id":"gpt-image-2"}]}`), nil
		}
		t.Errorf("unexpected Main request: %s %s", r.Method, r.URL.Path)
		return reply(404, `{}`), nil
	})
	return f
}

func (f *customFixture) call(method, path, body string) (int, string) {
	res := request(f.s, method, path, body, "fixture", f.s.PublicOrigin)
	return res.Code, res.Body.String()
}

type customPayload struct {
	Surface string
	BaseURL string
	Key     string
	Models  int
	Task    map[string]any
}

// connection asks for the own-key connection and checks what never varies.
func (f *customFixture) connection(t *testing.T, path, body, key string) customPayload {
	t.Helper()
	code, out := f.call("POST", path, body)
	var payload struct {
		Surface              string `json:"surface"`
		SelectedConnectionID string `json:"selectedConnectionId"`
		Connections          []struct {
			ID        string
			Protocol  string
			Transport map[string]string
		}
		Capabilities []struct {
			ID, ConnectionID, DisplayName, Protocol string
			Models                                  []map[string]any
			Task                                    map[string]any
		}
	}
	id := fmt.Sprintf("group-%d", studioCustomGroupID)
	if code != 200 || json.Unmarshal([]byte(out), &payload) != nil || len(payload.Connections) != 1 || len(payload.Capabilities) != 1 ||
		payload.SelectedConnectionID != id || payload.Connections[0].ID != id || payload.Capabilities[0].ID != id || payload.Capabilities[0].ConnectionID != id ||
		payload.Connections[0].Protocol != payload.Capabilities[0].Protocol {
		t.Fatal(code, out)
	}
	transport := payload.Connections[0].Transport
	// The origin follows current address settings and the key is the user's.
	binding, _ := f.s.Studio.customBinding(f.user)
	if moved, ok := f.moved[fmt.Sprint(binding.KeyID)]; ok {
		var now studioKey
		if json.Unmarshal([]byte(moved), &now) != nil {
			t.Fatal(moved)
		}
		binding, _ = studioCustomCurrent(binding, studioKeyPlatform(now))
	}
	origin := f.s.Studio.customAPIOrigin(binding)
	if !strings.HasPrefix(transport["baseUrl"], origin) || strings.ContainsAny(strings.TrimPrefix(transport["baseUrl"], origin), "@:?#") || transport["credentialRef"] != key {
		t.Fatal(out)
	}
	return customPayload{Surface: payload.Surface, BaseURL: transport["baseUrl"], Key: transport["credentialRef"], Models: len(payload.Capabilities[0].Models), Task: payload.Capabilities[0].Task}
}

func TestStudioCustomConnectionUsesOwnKeyOnTheFixedOrigin(t *testing.T) {
	dir := t.TempDir()
	f := newCustomFixture(t, dir)
	custom := fmt.Sprintf(`{"group_id":%d}`, studioCustomGroupID)
	restore := fmt.Sprintf(`{"group_id":%d,"restore_only":true}`, studioCustomGroupID)

	code, body := f.call("GET", "/api/studio/custom", "")
	var info struct {
		GroupID    int64             `json:"group_id"`
		Enabled    bool              `json:"enabled"`
		Configured bool              `json:"configured"`
		APIOrigin  string            `json:"api_origin"`
		Keys       []studioCustomKey `json:"keys"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &info) != nil || info.GroupID != studioCustomGroupID || !info.Enabled || info.Configured || info.APIOrigin != f.s.SubOrigin || len(info.Keys) != 4 {
		t.Fatal(code, body)
	}
	// Each key starts from the interface and prefix Main serves for its platform.
	starts := ""
	for _, key := range info.Keys {
		starts += fmt.Sprintf("%d:%s:%s:%s ", key.ID, key.Protocol, key.Prefix, key.Blocker)
	}
	if strings.Contains(body, "sk-") || info.Keys[0].Hint != "0011" ||
		starts != "11:openai_images:/v1: 12:gemini_native:/v1beta: 14:openai_images:/v1:media_engine_group 16:gemini_native:/antigravity/v1beta: " {
		t.Fatal("key list leaks a key or misdescribes one: " + starts + body)
	}
	for _, body := range []string{custom, restore} {
		if code, out := f.call("POST", "/api/studio/connection", body); code != 403 || !strings.Contains(out, "existing_authorization_required") {
			t.Fatal("connected before a key was chosen", code, out)
		}
	}

	// Models are listed under the chosen prefix, in either list shape.
	code, body = f.call("POST", "/api/studio/custom/models", `{"key_id":11,"protocol":"openai_images","prefix":"/ignored"}`)
	if code != 200 || body != `{"models":["gpt-image-2","gpt-5"]}`+"\n" {
		t.Fatal(code, body)
	}
	code, body = f.call("POST", "/api/studio/custom/models", `{"key_id":16,"protocol":"gemini_native","prefix":"/ignored"}`)
	if code != 200 || body != `{"models":["gemini-3-pro-image-preview","gemini-3-pro"]}`+"\n" {
		t.Fatal(code, body)
	}
	if strings.Join(f.models, "|") != "/v1/models Bearer sk-own-openai-0011|/antigravity/v1beta/models Bearer sk-own-antigr-0016" {
		t.Fatal(f.models)
	}
	for _, key := range []string{"13", "14", "15", "99"} {
		if code, _ := f.call("POST", "/api/studio/custom/models", `{"key_id":`+key+`,"protocol":"openai_images"}`); code != 403 {
			t.Fatalf("key %s listed models: %d", key, code)
		}
	}
	save := func(fields string) (int, string) { return f.call("PUT", "/api/studio/custom", `{`+fields+`}`) }
	for name, fields := range map[string]string{
		"unknown protocol":      `"key_id":11,"protocol":"grok_video","prefix":"/v1","models":["gpt-image-2"]`,
		"no models":             `"key_id":11,"protocol":"openai_images","prefix":"/v1","models":[]`,
		"typed path":            `"key_id":11,"protocol":"openai_images","prefix":"/v1","create_path":"/images/generations","models":["gpt-image-2"]`,
		"host field":            `"key_id":11,"protocol":"openai_images","prefix":"/v1","base_url":"https://elsewhere.example","models":["gpt-image-2"]`,
		"key instead of key id": `"api_key":"sk-own-openai-0011","protocol":"openai_images","prefix":"/v1","models":["gpt-image-2"]`,
	} {
		if code, out := save(fields); code != 400 {
			t.Fatalf("%s accepted: %d %s", name, code, out)
		}
	}
	for _, key := range []string{"13", "14", "15", "99"} {
		if code, _ := save(`"key_id":` + key + `,"protocol":"openai_images","prefix":"/v1","models":["gpt-image-2"]`); code != 403 {
			t.Fatalf("key %s saved: %d", key, code)
		}
	}

	// The default prefix is the contract every registered group uses, for both clients.
	if code, out := save(`"key_id":11,"protocol":"openai_images","models":["gpt-image-2","gpt-5"]`); code != 200 || !strings.Contains(out, `"configured":true`) {
		t.Fatal(code, out)
	}
	if _, out := f.call("GET", "/api/studio/groups", ""); !strings.Contains(out, `"configured":true`) || !strings.Contains(out, fmt.Sprintf(`"group_id":%d`, studioCustomGroupID)) {
		t.Fatal("group list does not offer the saved connection: " + out)
	}
	registered := fmt.Sprint(studioTask("sub2api", "openai_images"))
	for path, surface := range map[string]string{"/api/studio/connection": "image_studio", "/api/canvas/connection": "media"} {
		got := f.connection(t, path, custom, "sk-own-openai-0011")
		if got.Surface != surface || got.BaseURL != f.s.SubOrigin || got.Models != 2 || fmt.Sprint(got.Task) != registered {
			t.Fatal(path, got)
		}
	}
	if got := f.connection(t, "/api/canvas/connection", restore, "sk-own-openai-0011"); got.Surface != "media" {
		t.Fatal(got)
	}

	// An old file with a user-typed prefix retains both clients' old contracts.
	legacy := `{"9":{"key_id":11,"protocol":"openai_images","prefix":"/alt/v1","models":["gpt-image-2"]}}`
	if e := os.WriteFile(filepath.Join(dir, "studio-custom.json"), []byte(legacy), 0600); e != nil {
		t.Fatal(e)
	}
	var loadErr error
	f.s.Studio, loadErr = OpenStudio(dir, f.s.SubOrigin, f.s.Studio.Capabilities, "")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	got := f.connection(t, "/api/studio/connection", custom, "sk-own-openai-0011")
	if got.BaseURL != f.s.SubOrigin+"/alt/v1" || got.Task["createPath"] != "/images/generations/async" || got.Task["editPath"] != "/images/edits/async" || fmt.Sprint(got.Task["modes"]) != "[async]" {
		t.Fatal(got)
	}
	got = f.connection(t, "/api/canvas/connection", custom, "sk-own-openai-0011")
	if got.BaseURL != f.s.SubOrigin || got.Task["createPath"] != "/alt/v1/images/generations" || got.Task["editPath"] != "/alt/v1/images/edits" || fmt.Sprint(got.Task["modes"]) != "[sync]" || got.Task["statusPathTemplate"] != nil {
		t.Fatal(got)
	}

	// The native Gemini interface under the Antigravity prefix, for both clients.
	if code, out := save(`"key_id":16,"protocol":"gemini_native","prefix":"/antigravity/v1beta","models":["gemini-3-pro-image-preview"]`); code != 200 {
		t.Fatal(code, out)
	}
	for _, path := range []string{"/api/studio/connection", "/api/canvas/connection"} {
		got = f.connection(t, path, custom, "sk-own-antigr-0016")
		if got.BaseURL != f.s.SubOrigin || got.Task["createPath"] != "/antigravity/v1beta/models/{model}:generateContent" || got.Task["editPath"] != got.Task["createPath"] || fmt.Sprint(got.Task["modes"]) != "[sync]" {
			t.Fatal(path, got)
		}
	}
	if code, out := save(`"key_id":11,"protocol":"openai_images","prefix":"/v1","models":["gpt-image-2","gpt-5"]`); code != 200 {
		t.Fatal(code, out)
	}

	// Stored by key ID only, kept across a restart, and one user's is not another's.
	disk, _ := os.ReadFile(filepath.Join(dir, "studio-custom.json"))
	if strings.Contains(string(disk), "sk-") || !strings.Contains(string(disk), `"key_id": 11`) || !strings.Contains(string(disk), `"prefix": "/v1"`) {
		t.Fatal(string(disk))
	}
	var e error
	if f.s.Studio, e = OpenStudio(dir, f.s.SubOrigin, f.s.Studio.Capabilities, ""); e != nil {
		t.Fatal(e)
	}
	f.connection(t, "/api/studio/connection", restore, "sk-own-openai-0011")
	f.user = 10
	if code, _ := f.call("POST", "/api/studio/connection", custom); code != 403 {
		t.Fatal("another user's connection was offered", code)
	}
	f.user = 9

	// A revoked key is reported, never replaced.
	f.revoked = true
	if code, out := f.call("POST", "/api/studio/connection", custom); code != 403 || !strings.Contains(out, "studio_authorization_unavailable") {
		t.Fatal(code, out)
	}
	f.revoked = false

	// An administrator can turn the feature off without removing what users saved.
	f.role = "admin"
	if code, out := f.call("PUT", "/api/studio/admin/registry", `{"revision":0,"custom_disabled":true,"groups":[]}`); code != 200 || !strings.Contains(out, `"custom_disabled":true`) {
		t.Fatal(code, out)
	}
	f.role = "user"
	if code, out := f.call("GET", "/api/studio/custom", ""); code != 200 || !strings.Contains(out, `"enabled":false`) || !strings.Contains(out, `"configured":false`) || strings.Contains(out, `"keys"`) {
		t.Fatal(code, out)
	}
	if code, _ := save(`"key_id":11,"protocol":"openai_images","prefix":"/v1","models":["gpt-image-2"]`); code != 403 {
		t.Fatal("saved while disabled", code)
	}
	if code, _ := f.call("POST", "/api/studio/custom/models", `{"key_id":11,"protocol":"openai_images","prefix":"/ignored"}`); code != 403 {
		t.Fatal("listed models while disabled", code)
	}
	if code, _ := f.call("POST", "/api/studio/connection", custom); code != 403 {
		t.Fatal("connected while disabled", code)
	}
	f.role = "admin"
	if code, out := f.call("PUT", "/api/studio/admin/registry", `{"revision":1,"custom_disabled":false,"groups":[]}`); code != 200 {
		t.Fatal(code, out)
	}
	f.role = "user"
	f.connection(t, "/api/studio/connection", custom, "sk-own-openai-0011")

	if code, _ := f.call("DELETE", "/api/studio/custom", ""); code != 204 {
		t.Fatal("delete", code)
	}
	if code, _ := f.call("DELETE", "/api/studio/custom", ""); code != 204 {
		t.Fatal("repeated delete", code)
	}
	if code, _ := f.call("POST", "/api/studio/connection", custom); code != 403 {
		t.Fatal("connected after delete", code)
	}
	if f.created != 0 {
		t.Fatal("a custom connection created a key")
	}
}

func TestStudioCustomDefaultsFollowThePlatform(t *testing.T) {
	for platform, want := range map[string]string{"openai": "openai_images /v1", "grok": "grok_image /v1", "gemini": "gemini_native /v1beta", "antigravity": "gemini_native /antigravity/v1beta", "composite": "openai_images /v1", "": "openai_images /v1"} {
		if protocol, prefix := studioCustomDefaults(platform); protocol+" "+prefix != want {
			t.Fatal(platform, protocol, prefix)
		}
	}
	// An empty prefix is the interface's usual one.
	gemini, e := studioCustomBinding(StudioCustomBinding{KeyID: 12, Protocol: "gemini_native", Models: []string{"gemini-3-pro-image-preview"}})
	if base, task := studioCustomTask(gemini, false); e != nil || gemini.Prefix != "/v1beta" || base != "" || fmt.Sprint(task) != fmt.Sprint(studioTask("sub2api", "gemini_native")) {
		t.Fatal(e, gemini, base, task)
	}
	grok, e := studioCustomBinding(StudioCustomBinding{KeyID: 12, Protocol: "grok_image", Models: []string{"grok-imagine-image"}})
	if base, task := studioCustomTask(grok, true); e != nil || grok.Prefix != "/v1" || base != "" || fmt.Sprint(task) != fmt.Sprint(studioTask("sub2api", "grok_image")) {
		t.Fatal(e, grok, base, task)
	}
}

func TestStudioCustomRejectsDamagedState(t *testing.T) {
	for name, content := range map[string]string{
		"not json":   `[`,
		"bad user":   `{"0":{"key_id":1,"protocol":"gemini_native","prefix":"/v1beta","models":["m"]}}`,
		"bad prefix": `{"9":{"key_id":1,"protocol":"openai_images","prefix":"//x/v1","models":["m"]}}`,
	} {
		dir := t.TempDir()
		if e := os.WriteFile(filepath.Join(dir, "studio-custom.json"), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := OpenStudio(dir, "https://main.example", nil, ""); e == nil {
			t.Fatalf("%s: started on damaged custom connections", name)
		}
	}
}

func TestGatewayModelsReportsRefusalAndOutage(t *testing.T) {
	s := testServer(t, "user")
	status := 401
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://main.example/v1/models" {
			t.Errorf("model listing went to %s", r.URL)
		}
		return reply(status, `{"data":"not a list"}`), nil
	})
	source := httptest.NewRequest("GET", "/", nil)
	if _, e := s.gatewayModels(source, "sk-fixture", "/v1"); e != ErrCredential {
		t.Fatal(e)
	}
	status = 502
	if _, e := s.gatewayModels(source, "sk-fixture", "/v1"); e != ErrTransport {
		t.Fatal(e)
	}
	status = 200
	if _, e := s.gatewayModels(source, "sk-fixture", "/v1"); e != ErrResponse {
		t.Fatal(e)
	}
	if _, e := s.gatewayModels(source, "sk fixture\r\nX: y", "/v1"); e != ErrCredential {
		t.Fatal("malformed key reached the gateway", e)
	}
	if _, e := s.gatewayModels(source, "sk-fixture", "//elsewhere.example/v1"); e != ErrCredential {
		t.Fatal("malformed prefix reached the gateway", e)
	}
}
