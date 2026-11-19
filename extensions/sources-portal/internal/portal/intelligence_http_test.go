package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type intelligenceNativeFixture struct {
	mu         sync.Mutex
	role       string
	keys       map[int64]intelligenceNativeKey
	creates    int
	updates    int
	loseCreate bool
	lastCreate map[string]any
	groups     []StudioGroup
}

func intelligenceAPIFixture(t *testing.T) (*Server, *intelligenceNativeFixture) {
	t.Helper()
	s := testServer(t, "admin")
	store, err := OpenIntelligenceStore(s.Service.Store)
	if err != nil {
		t.Fatal(err)
	}
	s.Intelligence, err = NewIntelligenceService(store, s.SubOrigin, testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	native := &intelligenceNativeFixture{role: "admin", keys: map[int64]intelligenceNativeKey{}, groups: []StudioGroup{{ID: 17, Name: "Codex fixture", Platform: "openai", Status: "active"}}}
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		native.mu.Lock()
		defer native.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-admin" && r.Header.Get("Authorization") != "Bearer fixture-user" {
			return reply(401, ""), nil
		}
		respond := func(value any) (*http.Response, error) {
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": value})
			return reply(200, string(raw)), nil
		}
		switch r.URL.Path {
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") == "Bearer fixture-user" {
				return respond(Identity{ID: 10, Role: "user", Status: "active"})
			}
			return respond(Identity{ID: 9, Role: native.role, Status: "active"})
		case "/api/v1/groups/available":
			return respond(native.groups)
		case "/api/v1/keys":
			if r.Method == "GET" {
				items := []intelligenceNativeKey{}
				for _, key := range native.keys {
					if strings.Contains(key.Name, r.URL.Query().Get("search")) {
						items = append(items, key)
					}
				}
				return respond(map[string]any{"items": items})
			}
			if r.Method != "POST" {
				t.Fatal("unexpected native key operation")
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &native.lastCreate)
			native.creates++
			expires := time.Now().Add(30 * 24 * time.Hour)
			key := intelligenceNativeKey{ID: int64(native.creates), UserID: 9, GroupID: int64(native.lastCreate["group_id"].(float64)), Name: native.lastCreate["name"].(string), Key: "sk-private-native-key", Status: "active", DailyLimit: native.lastCreate["rate_limit_1d"].(float64), Quota: native.lastCreate["quota"].(float64), ExpiresAt: &expires}
			native.keys[key.ID] = key
			if native.loseCreate {
				return nil, errors.New("lost response after key was created")
			}
			return respond(key)
		default:
			if !mainKeyPath.MatchString(r.URL.Path) {
				t.Fatalf("unexpected native API %s", r.URL.Path)
			}
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v1/keys/"), 10, 64)
			key, ok := native.keys[id]
			if !ok {
				return reply(404, ""), nil
			}
			if r.Method == "PUT" {
				var in map[string]float64
				_ = json.NewDecoder(r.Body).Decode(&in)
				if len(in) != 2 {
					t.Fatal("limit update must not reset usage or extend expiry")
				}
				key.Quota, key.DailyLimit = in["quota"], in["rate_limit_1d"]
				native.keys[id] = key
				native.updates++
			}
			return respond(key)
		}
	})
	return s, native
}

func intelligenceInputJSON(revision int64, enabled bool) string {
	return fmt.Sprintf(`{"revision":%d,"group_id":17,"name":"Codex fixture","model":"gpt-6-astra","reasoning_effort":"medium","enabled":%t,"candy_interval_seconds":300,"pelican_interval_seconds":1200,"daily_limit":20,"quota":200,"expires_in_days":30}`, revision, enabled)
}

func TestIntelligenceNativeKeyProvisioningAndPrivateSnapshot(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	response := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	if response.Code != 200 {
		t.Fatalf("create plan: %d %s", response.Code, response.Body.String())
	}
	var plan IntelligencePlan
	_ = json.Unmarshal(response.Body.Bytes(), &plan)
	if native.creates != 1 || native.lastCreate["quota"] != float64(200) || native.lastCreate["rate_limit_1d"] != float64(20) || native.lastCreate["expires_in_days"] != float64(30) || !plan.Enabled {
		t.Fatal("native scoped budget defaults were not applied")
	}
	for _, path := range []string{"/api/intelligence-tests/snapshot", "/api/intelligence-tests/settings"} {
		res := request(s, "GET", path, "", "fixture-admin", "")
		if res.Code != 200 || strings.Contains(res.Body.String(), "sk-private") || strings.Contains(res.Body.String(), "encrypted_key") || strings.Contains(res.Body.String(), "key_operation") {
			t.Fatalf("private credential escaped through %s", path)
		}
	}
	again := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	if again.Code != 409 || native.creates != 1 {
		t.Fatal("duplicate group silently created another key")
	}
	edit := strings.Replace(intelligenceInputJSON(plan.Revision, false), `"daily_limit":20`, `"daily_limit":10`, 1)
	res := request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, edit, "fixture-admin", s.PublicOrigin)
	if res.Code != 200 || native.creates != 1 || native.updates != 1 {
		t.Fatal("native limit edit failed without preserving the original key")
	}
	stale := request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, edit, "fixture-admin", s.PublicOrigin)
	if stale.Code != 409 || native.updates != 1 {
		t.Fatal("stale settings mutated the native key")
	}
}

func TestIntelligenceLostKeyResponseReconcilesWithoutRecreate(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	native.loseCreate = true
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	if res.Code != 503 || native.creates != 1 {
		t.Fatal("expected indeterminate key response")
	}
	var plan IntelligencePlan
	for _, row := range s.Intelligence.Store.snapshot().Plans {
		plan = row.Plan
	}
	native.loseCreate = false
	res = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, intelligenceInputJSON(plan.Revision, true), "fixture-admin", s.PublicOrigin)
	if res.Code != 200 || native.creates != 1 {
		t.Fatalf("existing key should be recovered by exact name, not created again: %d", res.Code)
	}
}

func TestIntelligenceAdminOnlyProtectsDataAndResults(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	_ = json.Unmarshal(res.Body.Bytes(), &plan)
	run, err := s.Intelligence.Enqueue(plan.ID, "candy", "private-result-operation", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Intelligence.Store.saveResult(run.ID, IntelligenceResult{Answer: "private answer"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		r := st.Runs[run.ID]
		r.Run.HasResult = true
		st.Runs[run.ID] = r
		return nil
	})
	for _, path := range []string{"/api/intelligence-tests/snapshot", "/api/intelligence-tests/runs/" + run.ID, "/api/intelligence-tests/settings"} {
		for _, tc := range []struct {
			role, token string
			code        int
		}{{"admin", "", 401}, {"user", "fixture-admin", 403}, {"admin", "fixture-admin", 200}} {
			native.role = tc.role
			r := request(s, "GET", path+"?role=admin&user_id=9&public=true", "", tc.token, "")
			if r.Code != tc.code {
				t.Fatalf("%s %s: %d", path, tc.role, r.Code)
			}
			if r.Code != 200 && (strings.Contains(r.Body.String(), "private answer") || strings.Contains(r.Body.String(), "Codex fixture")) {
				t.Fatal("private data returned to forbidden reader")
			}
		}
	}
	for _, path := range []string{"/public/intelligence-tests", "/public/intelligence-tests/runs/" + run.ID} {
		if response := request(s, "GET", path, "", "fixture-admin", ""); response.Code != 401 {
			t.Fatal("private data crossed the public cache namespace")
		}
	}
	native.role = "admin"
	r := request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":0}`, "fixture-admin", s.PublicOrigin)
	if r.Code != 400 || !s.Intelligence.Store.snapshot().AdminOnly {
		t.Fatal("omitted visibility must not make results public")
	}
	r = request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":0,"viewer_access":"authenticated"}`, "fixture-admin", s.PublicOrigin)
	if r.Code != 200 {
		t.Fatal("explicit publication failed")
	}
	if request(s, "GET", "/public/intelligence-tests", "", "", "").Code != 401 {
		t.Fatal("anonymous publication is forbidden")
	}
	if request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "", s.PublicOrigin).Code != 401 {
		t.Fatal("public read mode widened management access")
	}
}

func TestIntelligenceSettingsRejectUnlimitedAndUnsupportedTargets(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	for _, body := range []string{
		strings.Replace(intelligenceInputJSON(0, true), `"quota":200`, `"quota":-1`, 1),
		strings.Replace(intelligenceInputJSON(0, true), `"daily_limit":20`, `"daily_limit":0`, 1),
		strings.Replace(intelligenceInputJSON(0, true), "gpt-6-astra", "unverified-model", 1),
		strings.Replace(intelligenceInputJSON(0, true), `"group_id":17`, `"group_id":99`, 1),
		strings.TrimSuffix(intelligenceInputJSON(0, true), "}") + `,"base_url":"https://evil.example"}`,
	} {
		res := request(s, "POST", "/api/intelligence-tests/plans", body, "fixture-admin", s.PublicOrigin)
		if res.Code != 400 && res.Code != 403 {
			t.Fatalf("unsafe config accepted: %d", res.Code)
		}
	}
	if native.creates != 0 {
		t.Fatal("invalid config created credentials")
	}
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", "https://evil.example")
	if res.Code != 403 || native.creates != 0 {
		t.Fatal("foreign origin created credentials")
	}
}
