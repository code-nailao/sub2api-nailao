package portal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIntelligenceAPIOriginConfig(t *testing.T) {
	for _, raw := range []string{"", " \n ", `{}`, `{"168":"HTTPS://API-B.EXAMPLE.COM:443/"}`, `{"168":"https://api-b.example.com","8":"https://public.example:8443"}`} {
		t.Run(raw, func(t *testing.T) {
			origins, err := intelligenceAPIOrigins(raw)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(raw, "168") && origins[168] != "https://api-b.example.com/v1" {
				t.Fatalf("incorrect canonical origin: %v", origins)
			}
			if strings.Contains(raw, `"8"`) && origins[8] != "https://public.example:8443/v1" {
				t.Fatalf("incorrect HTTPS port: %v", origins)
			}
		})
	}
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf(`"%d":"https://public.example"`, i+1)
	}
	for _, raw := range []string{
		`null`, `[]`, `"https://public.example"`, `{`, `{} {}`, `{} true`,
		`{"168":"https://public.example","168":"https://other.example"}`,
		`{"0":"https://public.example"}`, `{"-1":"https://public.example"}`,
		`{"0168":"https://public.example"}`, `{"+168":"https://public.example"}`,
		`{" 168":"https://public.example"}`, `{"1.68e2":"https://public.example"}`,
		`{"9223372036854775808":"https://public.example"}`,
		`{"168":null}`, `{"168":168}`, `{"168":[]}`, `{"168":{}}`,
		`{"168":""}`, `{"168":"http://public.example"}`, `{"168":"//public.example"}`,
		`{"168":"https://user:secret@public.example"}`, `{"168":"https://public.example/v1"}`,
		`{"168":"https://public.example/?x=1"}`, `{"168":"https://public.example?"}`,
		`{"168":"https://public.example/#fragment"}`, `{"168":"https://public.example#"}`,
		`{"168":"https://public.example/%2f"}`, `{"168":"https://public.example:"}`,
		`{"168":"https://public.example:0"}`, `{"168":"https://public.example:65536"}`,
		`{"168":"https://localhost"}`, `{"168":"https://127.0.0.1"}`, `{"168":"https://10.0.0.1"}`,
		`{"168":"https://[::1]"}`, `{"168":"https://169.254.169.254"}`,
		`{"168":"https://bad_name.example"}`, `{"168":"https://.example"}`, `{"168":"https://internal"}`,
		`{"168":"https://bad..example"}`, `{"168":"https://-bad.example"}`,
		`{"168":"https://public.example\\@private.example"}`,
		"{" + strings.Join(tooMany, ",") + "}", strings.Repeat(" ", 8193),
	} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			if _, err := intelligenceAPIOrigins(raw); err != errIntelligenceAPIOrigins {
				t.Fatalf("invalid deployment config accepted or echoed: %v", err)
			}
		})
	}
}

func TestIntelligenceAPIOriginSubmissionAndDefault(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		group        int64
		want         string
	}{
		{"overridden", `{"168":"https://api-b.example.com/"}`, 168, "https://api-b.example.com/v1"},
		{"other-group", `{"168":"https://api-b.example.com/"}`, 17, "https://main.example/v1"},
		{"unconfigured", "", 168, "https://main.example/v1"},
	} {
		for _, benchmark := range []string{"candy", "pelican", "creative"} {
			t.Run(tc.name+"/"+benchmark, func(t *testing.T) {
				svc, provider, _, _ := intelligenceFixture(t)
				plan := seedIntelligencePlan(t, svc, true)
				if err := svc.Store.change(func(st *intelligenceState) error {
					row := st.Plans[plan.ID]
					row.Plan.GroupID = tc.group
					secret, err := svc.Store.encrypt(row.Plan, "sk-private-monitor-fixture")
					row.Secret = secret
					row.Plan.NextCandy, row.Plan.NextPelican = svc.now().Add(time.Hour), svc.now().Add(time.Hour)
					st.Plans[plan.ID] = row
					return err
				}); err != nil {
					t.Fatal(err)
				}
				before := svc.Store.snapshot()
				configured, err := NewIntelligenceServiceWithAPIOrigins(svc.Store, "https://main.example", testIntelligenceProviderOrigin, tc.config)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, svc.Store.snapshot()) {
					t.Fatal("deployment route config changed plan, key, schedule, quota or history")
				}
				// Per-group API routes never change the configured detection service.
				if route := configured.provider.(*intelligenceHTTPProvider).origin; route != testIntelligenceProviderOrigin {
					t.Fatal("detection service origin changed")
				}
				configured.provider, configured.now = provider, svc.now
				run := enqueueIntelligence(t, configured, plan, benchmark)
				configured.tick()
				if provider.submits != 1 || len(provider.submissions) != 1 {
					t.Fatal("expected exactly one submit")
				}
				got := provider.submissions[0]
				if got.BaseURL != tc.want || got.APIKey != "sk-private-monitor-fixture" || got.Model != plan.Model || got.Effort != plan.Effort || got.Benchmark != benchmark {
					t.Fatal("submission routing or unchanged model/key fields are incorrect")
				}
				row := configured.Store.snapshot().Runs[run.ID]
				if row.RemoteID == "" || row.Run.Status != "running" {
					t.Fatal("accepted remote ID was not persisted")
				}
				// A changed deployment config never replays known remote jobs.
				restarted, err := NewIntelligenceServiceWithAPIOrigins(svc.Store, "https://main.example", testIntelligenceProviderOrigin, `{"168":"https://another.example"}`)
				if err != nil {
					t.Fatal(err)
				}
				restarted.provider = provider
				restarted.now = func() time.Time { return svc.now().Add(30 * time.Second) }
				restarted.tick()
				if provider.submits != 1 || provider.polls != 1 {
					t.Fatal("reconfiguration replayed accepted task or failed to poll old ID")
				}
			})
		}
	}
}

func TestIntelligenceInvalidOriginDoesNotRecoverOrModifyStore(t *testing.T) {
	svc, _, _, _ := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	run := enqueueIntelligence(t, svc, plan, "candy")
	if err := svc.Store.change(func(st *intelligenceState) error {
		row := st.Runs[run.ID]
		row.Run.Status = "submitting"
		st.Runs[run.ID] = row
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := svc.Store.snapshot()
	if _, err := NewIntelligenceServiceWithAPIOrigins(svc.Store, "https://main.example", testIntelligenceProviderOrigin, `{"168":"http://unsafe.example"}`); err != errIntelligenceAPIOrigins {
		t.Fatal("invalid config did not stop initialization")
	}
	if !reflect.DeepEqual(before, svc.Store.snapshot()) {
		t.Fatal("invalid config changed interrupted-run state")
	}
}

func TestIntelligenceAPIOriginSettingsIdentityAndWriteIsolation(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	mainOrigin, publicOrigin := s.SubOrigin, s.PublicOrigin
	response := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var plan IntelligencePlan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	before := s.Intelligence.Store.snapshot()
	configured, err := NewIntelligenceServiceWithAPIOrigins(s.Intelligence.Store, s.SubOrigin, testIntelligenceProviderOrigin, `{"17":"https://direct.example"}`)
	if err != nil {
		t.Fatal(err)
	}
	s.Intelligence = configured
	baseTransport := s.AuthClient.Transport
	identityCalls := 0
	s.InternalSubOrigin = "http://identity.internal:8080"
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "identity.internal:8080" || r.URL.Scheme != "http" {
			t.Fatal("API override changed native identity/group/key endpoint")
		}
		identityCalls++
		return baseTransport.RoundTrip(r)
	})
	settings := request(s, "GET", "/api/intelligence-tests/settings", "", "fixture-admin", "")
	var out struct {
		Plans []intelligencePlanSettings `json:"plans"`
	}
	if settings.Code != 200 || json.Unmarshal(settings.Body.Bytes(), &out) != nil || len(out.Plans) != 1 || out.Plans[0].APIBaseURL != "https://direct.example/v1" || out.Plans[0].ID != plan.ID || identityCalls < 2 {
		t.Fatal("admin settings did not expose configured endpoint using original identity service", settings.Code)
	}
	if s.SubOrigin != mainOrigin || s.PublicOrigin != publicOrigin {
		t.Fatal("override changed public/same-origin identity boundary")
	}
	for _, token := range []string{"", "fixture-user"} {
		res := request(s, "GET", "/api/intelligence-tests/settings", "", token, "")
		if res.Code == 200 || strings.Contains(res.Body.String(), "direct.example") {
			t.Fatal("non-admin received deployment API endpoint")
		}
	}
	for _, field := range []string{"api_base_url", "base_url", "api_origin"} {
		body := strings.TrimSuffix(intelligenceInputJSON(plan.Revision, false), "}") + `,"` + field + `":"https://unreviewed.example/v1"}`
		res := request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin)
		if res.Code != 400 {
			t.Fatal("plan API accepted deployment-only endpoint field", field, res.Code)
		}
	}
	public := request(s, "GET", "/api/intelligence-tests/snapshot", "", "fixture-admin", "")
	if public.Code != 200 || strings.Contains(public.Body.String(), "api_base_url") || strings.Contains(public.Body.String(), "direct.example") {
		t.Fatal("deployment endpoint escaped into regular snapshot")
	}
	runtime := request(s, "GET", "/public/runtime", "", "", "")
	if runtime.Code != 200 || !strings.Contains(runtime.Body.String(), `"main_origin":"https://main.example"`) {
		t.Fatal("override changed runtime main-site origin")
	}
	if !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) || native.creates != 1 || native.updates != 0 {
		t.Fatal("endpoint read/write isolation changed existing plan, key, limits or history")
	}
	stored, err := json.Marshal(s.Intelligence.Store.snapshot())
	if err != nil || strings.Contains(string(stored), "api_base_url") || strings.Contains(string(stored), "direct.example") {
		t.Fatal("deployment endpoint persisted in business state")
	}
}

// testIntelligenceProviderOrigin stands in for the operator-configured
// detection service (INTELLIGENCE_PROVIDER_ORIGIN).
const testIntelligenceProviderOrigin = "https://detector.example"

func TestIntelligenceProviderOriginIsRequiredAndCanonical(t *testing.T) {
	svc, _, _, _ := intelligenceFixture(t)
	before := svc.Store.snapshot()
	for _, raw := range []string{"", " ", "http://detector.example", "https://user:pass@detector.example", "https://detector.example/api", "https://detector.example?x=1", "https://localhost", "https://127.0.0.1"} {
		if _, err := NewIntelligenceServiceWithAPIOrigins(svc.Store, "https://main.example", raw, ""); err != errIntelligenceProviderOrigin {
			t.Fatalf("provider origin %q: got %v", raw, err)
		}
	}
	if !reflect.DeepEqual(before, svc.Store.snapshot()) {
		t.Fatal("rejected provider configuration modified the store")
	}
	configured, err := NewIntelligenceService(svc.Store, "https://main.example", "HTTPS://Detector.Example:443/")
	if err != nil {
		t.Fatal(err)
	}
	if route := configured.provider.(*intelligenceHTTPProvider).origin; route != testIntelligenceProviderOrigin {
		t.Fatalf("provider origin not canonical: %s", route)
	}
}
