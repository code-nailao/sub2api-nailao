package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestPrivateIntelligencePlanCannotBeReadByMembers(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	native.groups[0].IsExclusive = true
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil || p.Visibility != "admin" {
		t.Fatal("new plan not private", r.Code)
	}
	run, e := s.Intelligence.Enqueue(p.ID, "candy", "private-plan-result-fixture", true)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Intelligence.Store.saveResult(run.ID, IntelligenceResult{Answer: "private answer"}); e != nil {
		t.Fatal(e)
	}
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		v := st.Runs[run.ID]
		v.Run.HasResult = true
		v.Run.Status = "succeeded"
		st.Runs[run.ID] = v
		return nil
	})
	if request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":0,"viewer_access":"authenticated"}`, "fixture-admin", s.PublicOrigin).Code != 200 {
		t.Fatal("page open")
	}
	snapshot := request(s, "GET", "/api/intelligence-tests/snapshot?manage=1&role=admin", "", "fixture-user", "")
	if snapshot.Code != 200 || strings.Contains(snapshot.Body.String(), p.ID) || strings.Contains(snapshot.Body.String(), run.ID) {
		t.Fatal("private snapshot disclosed")
	}
	path := "/api/intelligence-tests/runs/" + run.ID + "?role=admin"
	if request(s, "GET", path, "", "fixture-user", "").Code != 404 || request(s, "GET", path, "", "fixture-admin", "").Code != 200 {
		t.Fatal("private result boundary")
	}
	for _, visibility := range []string{"authenticated", "admin"} {
		body := strings.Replace(intelligenceInputJSON(p.Revision, false), "{", fmt.Sprintf(`{"visibility":%q,`, visibility), 1)
		r = request(s, "PUT", "/api/intelligence-tests/plans/"+p.ID, body, "fixture-admin", s.PublicOrigin)
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
			t.Fatal("visibility update", r.Code)
		}
		want := 404
		if visibility == "authenticated" {
			want = 200
		}
		if request(s, "GET", path, "", "fixture-user", "").Code != want {
			t.Fatal("result visibility did not follow plan")
		}
	}
	if native.creates != 1 || native.updates != 0 {
		t.Fatal("visibility changed keys or limits")
	}
	st := s.Intelligence.Store.snapshot()
	if !st.AdminOnly || st.ViewerAccess != "admin" || intelligenceViewerAccess(st) != "authenticated" {
		t.Fatal("rollback reader could expose private plans")
	}
	before := p.Visibility
	r = request(s, "PUT", "/api/intelligence-tests/plans/"+p.ID, intelligenceInputJSON(p.Revision, false), "fixture-admin", s.PublicOrigin)
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil || p.Visibility != before {
		t.Fatal("legacy client changed visibility")
	}
	if request(s, "PUT", "/api/intelligence-tests/plans/"+p.ID, strings.Replace(intelligenceInputJSON(p.Revision, false), "{", `{"visibility":"public",`, 1), "fixture-admin", s.PublicOrigin).Code != 400 {
		t.Fatal("anonymous visibility accepted")
	}
}

func TestIntelligenceRejectsDirectProviderCredentials(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	for _, extra := range []string{`"base_url":"https://private-provider.invalid/v1",`, `"api_key":"sk-real-provider-do-not-forward",`} {
		body := strings.Replace(intelligenceInputJSON(0, false), "{", "{"+extra, 1)
		if request(s, "POST", "/api/intelligence-tests/plans", body, "fixture-admin", s.PublicOrigin).Code != 400 {
			t.Fatal("raw provider credentials accepted")
		}
	}
	if native.creates != 0 {
		t.Fatal("invalid configuration created a key")
	}
}

func TestIntelligenceVisibilityMigrationAndLegacyRoundTrip(t *testing.T) {
	s, _ := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(r.Body.Bytes(), &p)
	// Actual042 storage had only the legacy page-level visibility.
	raw, e := os.ReadFile(s.Intelligence.Store.path)
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	delete(v, "viewer_policy")
	v["viewer_access"] = "authenticated"
	for _, value := range v["plans"].(map[string]any) {
		delete(value.(map[string]any)["plan"].(map[string]any), "visibility")
	}
	if e = writeJSONAtomic(s.Intelligence.Store.path, v); e != nil {
		t.Fatal(e)
	}
	upgraded, e := OpenIntelligenceStore(s.Service.Store)
	if e != nil {
		t.Fatal(e)
	}
	st := upgraded.snapshot()
	if intelligenceViewerAccess(st) != "authenticated" || st.Plans[p.ID].Plan.Visibility != "authenticated" || st.ViewerAccess != "admin" || !st.AdminOnly {
		t.Fatal("042 migration changed shared audience or rollback guard")
	}
	first, _ := json.Marshal(st)
	again, e := OpenIntelligenceStore(s.Service.Store)
	if e != nil {
		t.Fatal(e)
	}
	second, _ := json.Marshal(again.snapshot())
	if string(first) != string(second) {
		t.Fatal("reopen repeated migration")
	}
	// An old binary may drop fields it does not know. Its legacy fields remain private.
	raw, _ = os.ReadFile(upgraded.path)
	_ = json.Unmarshal(raw, &v)
	delete(v, "viewer_policy")
	for _, value := range v["plans"].(map[string]any) {
		delete(value.(map[string]any)["plan"].(map[string]any), "visibility")
	}
	if e = writeJSONAtomic(upgraded.path, v); e != nil {
		t.Fatal(e)
	}
	restored, e := OpenIntelligenceStore(s.Service.Store)
	if e != nil {
		t.Fatal(e)
	}
	if intelligenceViewerAccess(restored.snapshot()) != "admin" || restored.snapshot().Plans[p.ID].Plan.Visibility != "admin" {
		t.Fatal("legacy writer silently reopened private data")
	}
	if restored.snapshot().Plans[p.ID].Plan.KeyID != p.KeyID {
		t.Fatal("migration replaced key")
	}
}

func TestPrivatePlanSubmitsOnlyTheNativeScopedKey(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	provider := &intelligenceFake{}
	s.Intelligence.provider = provider
	native.groups[0].IsExclusive = true
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(r.Body.Bytes(), &p)
	run, err := s.Intelligence.Enqueue(p.ID, "pelican", "private-scope-native-key", true)
	if err != nil {
		t.Fatal(err)
	}
	s.Intelligence.submit(context.Background(), run.ID)
	if len(provider.submissions) != 1 {
		t.Fatal("submission missing")
	}
	sent := provider.submissions[0]
	if sent.BaseURL != s.Intelligence.baseURL || sent.APIKey != native.keys[p.KeyID].Key || sent.APIKey != "sk-private-native-key" {
		t.Fatal("credentials did not come from native scoped key")
	}
	if p.Visibility != "admin" || p.Enabled {
		t.Fatal("new test unexpectedly published or scheduled")
	}
}
