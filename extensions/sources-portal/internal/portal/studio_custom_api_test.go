package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStudioAPILabelValidationAndDeploymentParsing(t *testing.T) {
	for _, label := range []string{"a", "0", "api", "api2", "api-images", strings.Repeat("a", 63)} {
		if e := validateStudioAPILabels([]string{label}, label); e != nil {
			t.Fatal(label, e)
		}
	}
	for _, label := range []string{"", "API", "-api", "api-", "api.main", "api/path", "api:YOUR_PORT", "api_2", " api", strings.Repeat("a", 64), "图"} {
		if e := validateStudioAPILabels([]string{label}, label); !errors.Is(e, ErrStudioRegistry) {
			t.Fatal(label, e)
		}
	}
	for _, tc := range []struct {
		labels []string
		def    string
	}{
		{[]string{"api", "api"}, "api"}, {[]string{"api"}, "api2"}, {[]string{"api"}, ""}, {nil, "api"},
	} {
		if e := validateStudioAPILabels(tc.labels, tc.def); e != ErrStudioRegistry {
			t.Fatal(tc, e)
		}
	}
	labels := []string{}
	for i := range 17 {
		labels = append(labels, fmt.Sprintf("api%d", i))
	}
	if e := validateStudioAPILabels(labels, labels[0]); e != ErrStudioRegistry {
		t.Fatal(e)
	}
	if e := validateStudioAPILabels(labels[:16], labels[0]); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{"api,api", "API", "api,", ",api", " ", strings.Join(labels, ",")} {
		if _, e := ParseStudioCustomAPILabels(raw); e != ErrStudioRegistry {
			t.Fatal(raw, e)
		}
	}
	for raw, want := range map[string][]string{"": {}, "api, api2": {"api", "api2"}} {
		got, e := ParseStudioCustomAPILabels(raw)
		if e != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(got, e)
		}
	}
}

func TestStudioAPIRegistryPrecedencePersistenceAndConflict(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenStudio(dir, "https://main.example", nil, "")
	if e != nil {
		t.Fatal(e)
	}
	check := func(want []string, def, source string) {
		t.Helper()
		labels, d, src := s.customAPISettings()
		if !reflect.DeepEqual(labels, want) || d != def || src != source {
			t.Fatal(labels, d, src)
		}
		if len(labels) > 0 {
			labels[0] = "changed"
		} // Snapshot must not mutate settings.
	}
	check([]string{}, "", "none")
	s, e = OpenStudio(dir, s.APIOrigin, nil, "", []string{"api", "api2"})
	if e != nil {
		t.Fatal(e)
	}
	check([]string{"api", "api2"}, "api", "deployment")
	if e := s.saveRegistry(0, false, nil, 1, []string{"api2", "api"}, "api"); e != nil {
		t.Fatal(e)
	}
	check([]string{"api2", "api"}, "api", "online")
	if e := s.saveRegistry(0, false, nil, 1, []string{"api"}, "api"); e != ErrConflict {
		t.Fatal(e)
	}
	if e := s.saveRegistry(1, false, nil, 1, []string{"api"}, "api2"); e != ErrStudioRegistry {
		t.Fatal(e)
	}
	s, e = OpenStudio(dir, s.APIOrigin, nil, "", []string{"api"})
	if e != nil {
		t.Fatal(e)
	}
	check([]string{"api2", "api"}, "api", "online")
	if e := s.saveRegistry(1, false, nil, 1, nil, ""); e != nil {
		t.Fatal(e)
	}
	check([]string{"api"}, "api", "deployment")
	for _, invalid := range []string{
		`{"custom_api_labels":["API"],"custom_api_default":"API"}`,
		`{"custom_api_labels":["api","api"],"custom_api_default":"api"}`,
		`{"custom_api_labels":["api"],"custom_api_default":"api2"}`,
		`{"custom_api_labels":[],"custom_api_default":"api"}`,
	} {
		content := `{"revision":1,"groups":[],` + strings.TrimPrefix(invalid, "{")
		if e := os.WriteFile(filepath.Join(dir, "studio-registry.json"), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := OpenStudio(dir, s.APIOrigin, nil, ""); e != ErrStudioRegistry {
			t.Fatal(content, e)
		}
	}
}

func TestStudioCustomDerivedPath(t *testing.T) {
	for _, platform := range []string{"", "openai", "gemini", "grok", "composite", "antigravity"} {
		for _, protocol := range []string{"gemini_native", "openai_images", "grok_image", "unknown", ""} {
			got, e := studioCustomPathPrefix(platform, protocol)
			want := ""
			switch {
			case platform == "antigravity" && protocol == "gemini_native":
				want = "/antigravity/v1beta"
			case platform == "antigravity":
			case protocol == "gemini_native":
				want = "/v1beta"
			case protocol == "openai_images" || protocol == "grok_image":
				want = "/v1"
			}
			if got != want || (e == ErrStudioCustom) != (want == "") {
				t.Fatal(platform, protocol, got, e)
			}
		}
	}
}

func TestStudioCustomOriginResolution(t *testing.T) {
	s, e := OpenStudio(t.TempDir(), "https://main.example", nil, "", []string{"api", "api2"})
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ label, prefix, want string }{
		{"api", "/antigravity/v1beta", s.APIOrigin}, {"", "/alt/v1", s.APIOrigin},
		{"api2", "/v1", "https://api2.main.example"}, {"removed", "/v1", "https://api.main.example"},
	} {
		if got := s.customAPIOrigin(StudioCustomBinding{APILabel: tc.label, Prefix: tc.prefix}); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	s.deploymentLabels = nil
	if got := s.customAPIOrigin(StudioCustomBinding{APILabel: "removed", Prefix: "/v1"}); got != s.APIOrigin {
		t.Fatal(got)
	}
	for _, label := range []string{"api", "API", "api.main"} {
		content := fmt.Sprintf(`{"9":{"key_id":11,"protocol":"openai_images","prefix":"/alt/v1","models":["m"],"api_label":%q}}`, label)
		if e := os.WriteFile(s.customPath, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := OpenStudio(filepath.Dir(s.customPath), s.APIOrigin, nil, "")
		if (e == nil) != (label == "api") {
			t.Fatal(label, e)
		}
	}
}

func TestStudioCustomAPIHandlersAndConnectionPayloads(t *testing.T) {
	f := newCustomFixture(t, t.TempDir())
	f.s.Studio.deploymentLabels = []string{"api", "api2"}
	f.role = "admin"
	code, out := f.call("GET", "/api/studio/admin/registry", "")
	if code != 200 || !strings.Contains(out, `"deployment_api_labels":["api","api2"]`) || !strings.Contains(out, `"custom_api_source":"deployment"`) {
		t.Fatal(code, out)
	}
	for _, body := range []string{
		`{"revision":0,"custom_api_labels":["API"],"custom_api_default":"API"}`,
		`{"revision":0,"custom_api_labels":["api","api"],"custom_api_default":"api"}`,
		`{"revision":0,"custom_api_labels":["api"],"custom_api_default":"api2"}`,
		`{"revision":0,"custom_api_default":"api"}`,
	} {
		if code, out := f.call("PUT", "/api/studio/admin/registry", body); code != 400 {
			t.Fatal(code, out)
		}
	}
	body := `{"revision":0,"groups":[],"custom_api_labels":["api2","api"],"custom_api_default":"api2"}`
	if code, out := f.call("PUT", "/api/studio/admin/registry", body); code != 200 || !strings.Contains(out, `"custom_api_source":"online"`) || !strings.Contains(out, `"custom_api_default":"api2"`) {
		t.Fatal(code, out)
	}
	if code, _ := f.call("PUT", "/api/studio/admin/registry", body); code != 409 {
		t.Fatal(code)
	}
	f.role = "user"
	if code, _ := f.call("GET", "/api/studio/admin/registry", ""); code != 403 {
		t.Fatal(code)
	}
	if code, _ := f.call("PUT", "/api/studio/admin/registry", body); code != 403 {
		t.Fatal(code)
	}
	code, out = f.call("GET", "/api/studio/custom", "")
	if code != 200 || !strings.Contains(out, `"api_host":"main.example"`) || !strings.Contains(out, `"api_labels":["api2","api"]`) || !strings.Contains(out, `"api_default":"api2"`) || !strings.Contains(out, `"platform":"antigravity"`) {
		t.Fatal(code, out)
	}
	save := func(key int, protocol, label string) (int, string) {
		return f.call("PUT", "/api/studio/custom", fmt.Sprintf(`{"key_id":%d,"protocol":%q,"api_label":%q,"prefix":"/user/path","models":["m"]}`, key, protocol, label))
	}
	for _, label := range []string{"", "API", "missing", "api.main"} {
		if code, out := save(11, "openai_images", label); code != 400 {
			t.Fatal(code, out)
		}
	}
	for _, methodPath := range [][2]string{{"PUT", "/api/studio/custom"}, {"POST", "/api/studio/custom/models"}} {
		body := `{"key_id":16,"protocol":"openai_images"}`
		if methodPath[0] == "PUT" {
			body = `{"key_id":16,"protocol":"openai_images","api_label":"api","models":["m"]}`
		}
		if code, out := f.call(methodPath[0], methodPath[1], body); code != 400 || !strings.Contains(out, ErrStudioCustom.Error()) {
			t.Fatal(code, out)
		}
	}
	connectionBody := fmt.Sprintf(`{"group_id":%d}`, studioCustomGroupID)
	for _, tc := range []struct {
		key                                         int
		protocol, label, origin, prefix, credential string
	}{
		{11, "openai_images", "api", "https://api.main.example", "/v1", "sk-own-openai-0011"},
		{12, "gemini_native", "api2", "https://api2.main.example", "/v1beta", "sk-own-gemini-0012"},
		{16, "gemini_native", "api", f.s.SubOrigin, "/antigravity/v1beta", "sk-own-antigr-0016"},
	} {
		if code, out := save(tc.key, tc.protocol, tc.label); code != 200 {
			t.Fatal(code, out)
		}
		restarted, err := OpenStudio(filepath.Dir(f.s.Studio.customPath), f.s.SubOrigin, f.s.Studio.Capabilities, "", f.s.Studio.deploymentLabels)
		if err != nil {
			t.Fatal(err)
		}
		f.s.Studio = restarted
		b, _ := f.s.Studio.customBinding(9)
		if b.Prefix != tc.prefix || tc.key == 16 && b.APILabel != "" {
			t.Fatal(b)
		}
		code, out := f.call("GET", "/api/studio/custom", "")
		var info struct {
			Binding StudioCustomBinding `json:"binding"`
		}
		if code != 200 || json.Unmarshal([]byte(out), &info) != nil || info.Binding.Prefix != tc.prefix || info.Binding.APILabel != b.APILabel {
			t.Fatal(code, out)
		}
		for _, path := range []string{"/api/studio/connection", "/api/canvas/connection"} {
			if got := f.connection(t, path, connectionBody, tc.credential); got.BaseURL != tc.origin {
				t.Fatal(got)
			}
		}
		request := fmt.Sprintf(`{"key_id":%d,"protocol":%q,"prefix":"/ignored"}`, tc.key, tc.protocol)
		if code, out := f.call("POST", "/api/studio/custom/models", request); code != 200 {
			t.Fatal(code, out)
		}
		if got := f.models[len(f.models)-1]; !strings.HasPrefix(got, tc.prefix+"/models ") {
			t.Fatal(got)
		}
	}
	// An old binding stays on Main even when API subdomains are configured.
	legacy := StudioCustomBinding{KeyID: 11, Protocol: "openai_images", Prefix: "/alt/v1", Models: []string{"m"}}
	if err := f.s.Studio.saveCustom(9, &legacy); err != nil {
		t.Fatal(err)
	}
	if got := f.connection(t, "/api/studio/connection", connectionBody, "sk-own-openai-0011"); got.BaseURL != f.s.SubOrigin+"/alt/v1" {
		t.Fatal(got)
	}
	if got := f.connection(t, "/api/canvas/connection", connectionBody, "sk-own-openai-0011"); got.BaseURL != f.s.SubOrigin {
		t.Fatal(got)
	}
	// Removing a stored label redirects both clients to the current default, then to Main.
	if code, out := save(11, "openai_images", "api"); code != 200 {
		t.Fatal(code, out)
	}
	f.role = "admin"
	for _, tc := range []struct {
		revision            int
		labels, def, origin string
	}{
		{1, `["api2"]`, "api2", "https://api2.main.example"},
		{2, `[]`, "", "https://api.main.example"},
	} {
		body := fmt.Sprintf(`{"revision":%d,"groups":[],"custom_api_labels":%s,"custom_api_default":%q}`, tc.revision, tc.labels, tc.def)
		if code, out := f.call("PUT", "/api/studio/admin/registry", body); code != 200 {
			t.Fatal(code, out)
		}
		for _, path := range []string{"/api/studio/connection", "/api/canvas/connection"} {
			if got := f.connection(t, path, connectionBody, "sk-own-openai-0011"); got.BaseURL != tc.origin {
				t.Fatal(got)
			}
		}
	}
	f.s.Studio.deploymentLabels = nil
	for _, path := range []string{"/api/studio/connection", "/api/canvas/connection"} {
		if got := f.connection(t, path, connectionBody, "sk-own-openai-0011"); got.BaseURL != f.s.SubOrigin {
			t.Fatal(got)
		}
	}
}
