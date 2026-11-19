package portal

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIntelligenceExistingUnlimitedKeyResumesAndRuns(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &plan) != nil {
		t.Fatal("create fixture", res.Code)
	}
	key := native.keys[plan.KeyID]
	key.ExpiresAt, key.Quota, key.QuotaUsed, key.DailyLimit = nil, 0, 35.47, 100
	native.keys[plan.KeyID] = key
	body := strings.ReplaceAll(intelligenceInputJSON(plan.Revision, true), `"daily_limit":20`, `"daily_limit":100`)
	body = strings.Replace(body, `"quota":200`, `"quota":0`, 1)
	res = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin)
	plan = IntelligencePlan{}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &plan) != nil {
		t.Fatalf("unlimited native key must resume: %d %s", res.Code, res.Body.String())
	}
	if !plan.Enabled || plan.KeyStatus != "active" || plan.ExpiresAt != nil || plan.Quota != 0 || plan.DailyLimit != 100 || native.updates != 0 || native.creates != 1 || native.keys[plan.KeyID].QuotaUsed != 35.47 {
		t.Fatal("native key limits, usage or identity changed")
	}
	fake := &intelligenceFake{}
	s.Intelligence.provider = fake
	now := time.Now().Add(365 * 24 * time.Hour)
	s.Intelligence.now = func() time.Time { return now }
	s.Intelligence.tick()
	state := s.Intelligence.Store.snapshot()
	if !state.Plans[plan.ID].Plan.Enabled || state.Plans[plan.ID].Plan.KeyStatus != "active" || fake.submits != 2 {
		t.Fatal("scheduler or submission treated a non-expiring key as expired")
	}
	if _, err := NewIntelligenceService(s.Intelligence.Store, s.SubOrigin, testIntelligenceProviderOrigin); err != nil {
		t.Fatal("unlimited plan cannot be reopened", err)
	}
}

func TestIntelligenceKeyLimitsStillRejectUnavailableKeys(t *testing.T) {
	for _, reason := range []string{"expired", "exhausted", "disabled", "daily_unlimited"} {
		t.Run(reason, func(t *testing.T) {
			s, native := intelligenceAPIFixture(t)
			r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
			var plan IntelligencePlan
			if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &plan) != nil {
				t.Fatal("fixture")
			}
			key := native.keys[plan.KeyID]
			switch reason {
			case "expired":
				at := time.Now().Add(-time.Hour)
				key.ExpiresAt = &at
			case "exhausted":
				key.QuotaUsed = key.Quota
			case "disabled":
				key.Status = "inactive"
			case "daily_unlimited":
				key.DailyLimit = 0
			}
			native.keys[plan.KeyID] = key
			body := intelligenceInputJSON(plan.Revision, true)
			if reason == "daily_unlimited" {
				body = strings.Replace(body, `"daily_limit":20`, `"daily_limit":0`, 1)
			}
			before := s.Intelligence.Store.snapshot()
			r = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin)
			want := 403
			if reason == "daily_unlimited" {
				want = 400
			}
			if r.Code != want || !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
				t.Fatal("unavailable key resumed", r.Code)
			}
		})
	}
}

func TestIntelligencePauseWithDeletedNativeKeyPreservesInFlight(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &plan) != nil {
		t.Fatal("fixture")
	}
	if _, err := s.Intelligence.Enqueue(plan.ID, "candy", "existing-queued-test", true); err != nil {
		t.Fatal(err)
	}
	delete(native.keys, plan.KeyID)
	native.groups = nil
	before := s.Intelligence.Store.snapshot()
	body := intelligenceInputJSON(plan.Revision, false)
	original := s.AuthClient.Transport
	configCalls := 0
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/groups/available" || strings.HasPrefix(r.URL.Path, "/api/v1/keys") {
			configCalls++
		}
		return original.RoundTrip(r)
	})
	r = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin)
	if r.Code != 200 {
		t.Fatalf("pause must remain possible with deleted key: %d %s", r.Code, r.Body.String())
	}
	after := s.Intelligence.Store.snapshot()
	if after.Plans[plan.ID].Plan.Enabled || configCalls != 0 || !reflect.DeepEqual(before.Runs, after.Runs) || !reflect.DeepEqual(before.Requests, after.Requests) {
		t.Fatal("pause modified active work or called unavailable native key")
	}
	changed, prior := after.Plans[plan.ID], before.Plans[plan.ID]
	changed.Plan.Enabled, changed.Plan.Revision, changed.Plan.UpdatedAt = prior.Plan.Enabled, prior.Plan.Revision, prior.Plan.UpdatedAt
	if !reflect.DeepEqual(changed, prior) {
		t.Fatal("pause changed credentials, schedule or limits")
	}
}

func TestIntelligencePauseShortcutDoesNotBypassAuthorization(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &plan) != nil {
		t.Fatal("fixture")
	}
	native.groups = nil
	body := intelligenceInputJSON(plan.Revision, false)
	for _, tc := range []struct {
		name, body, token, origin string
		status                    int
	}{
		{"member", body, "fixture-user", s.PublicOrigin, 403},
		{"foreign_origin", body, "fixture-admin", "https://untrusted.invalid", 403},
		{"stale", intelligenceInputJSON(plan.Revision-1, false), "fixture-admin", s.PublicOrigin, 409},
		{"change_group", strings.Replace(body, `"group_id":17`, `"group_id":99`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"change_quota", strings.Replace(body, `"quota":200`, `"quota":0`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"change_schedule", strings.Replace(body, `"candy_interval_seconds":300`, `"candy_interval_seconds":60`, 1), "fixture-admin", s.PublicOrigin, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := s.Intelligence.Store.snapshot()
			r := request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, tc.body, tc.token, tc.origin)
			if r.Code != tc.status || !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
				t.Fatalf("expected %d, got %d", tc.status, r.Code)
			}
		})
	}
}
