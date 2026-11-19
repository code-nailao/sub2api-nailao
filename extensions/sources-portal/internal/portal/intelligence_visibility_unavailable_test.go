package portal

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIntelligenceVisibilityOnlySaveWithUnavailableGroupOrKey(t *testing.T) {
	for _, unavailable := range []string{"missing_group", "disabled_group", "missing_key", "disabled_key", "expired_key", "exhausted_key"} {
		t.Run(unavailable, func(t *testing.T) {
			s, native := intelligenceAPIFixture(t)
			response := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
			var plan IntelligencePlan
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &plan) != nil {
				t.Fatal("fixture creation", response.Code)
			}
			if err := s.Intelligence.Store.change(func(st *intelligenceState) error {
				row := st.Plans[plan.ID]
				row.Plan.Visibility = "authenticated"
				st.Plans[plan.ID] = row
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			switch unavailable {
			case "missing_group":
				native.groups = nil
			case "disabled_group":
				native.groups[0].Status = "disabled"
			case "missing_key":
				delete(native.keys, plan.KeyID)
			default:
				key := native.keys[plan.KeyID]
				if unavailable == "disabled_key" {
					key.Status = "disabled"
				} else if unavailable == "expired_key" {
					expired := time.Now().Add(-time.Hour)
					key.ExpiresAt = &expired
				} else {
					key.QuotaUsed = key.Quota
				}
				native.keys[plan.KeyID] = key
			}
			originalTransport := s.AuthClient.Transport
			nativeConfigReads := 0
			s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/groups/available" || strings.HasPrefix(r.URL.Path, "/api/v1/keys") {
					nativeConfigReads++
				}
				return originalTransport.RoundTrip(r)
			})
			for _, visibility := range []string{"admin", "authenticated"} {
				before := s.Intelligence.Store.snapshot()
				record := before.Plans[plan.ID]
				body := strings.Replace(intelligenceInputJSON(record.Plan.Revision, true), "{", `{"visibility":"`+visibility+`",`, 1)
				response = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin)
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &plan) != nil {
					t.Fatalf("visibility-only save must not require a usable group/key: %d %s", response.Code, response.Body.String())
				}
				after := s.Intelligence.Store.snapshot()
				changed := after.Plans[plan.ID]
				if plan.Visibility != visibility || plan.Revision != record.Plan.Revision+1 {
					t.Fatal("visibility/revision not updated")
				}
				changed.Plan.Visibility = record.Plan.Visibility
				changed.Plan.Revision = record.Plan.Revision
				changed.Plan.UpdatedAt = record.Plan.UpdatedAt
				if !reflect.DeepEqual(changed, record) || !reflect.DeepEqual(before.Runs, after.Runs) || !reflect.DeepEqual(before.Requests, after.Requests) || before.SettingsRevision != after.SettingsRevision {
					t.Fatal("visibility changed scheduling, credentials, limits, history or page access")
				}
			}
			if nativeConfigReads != 0 || native.creates != 1 || native.updates != 0 {
				t.Fatal("visibility-only save accessed native group/key configuration")
			}
		})
	}
}

func TestIntelligenceVisibilityShortcutRetainsAuthorizationAndExecutionValidation(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &plan) != nil {
		t.Fatal("fixture creation")
	}
	native.groups = nil
	body := strings.Replace(intelligenceInputJSON(plan.Revision, false), "{", `{"visibility":"authenticated",`, 1)
	staleBody := strings.Replace(intelligenceInputJSON(plan.Revision-1, false), "{", `{"visibility":"authenticated",`, 1)
	for _, tc := range []struct {
		name, body, token, origin string
		status                    int
	}{
		{"member", body, "fixture-user", s.PublicOrigin, 403},
		{"foreign_origin", body, "fixture-admin", "https://untrusted.invalid", 403},
		{"stale_revision", staleBody, "fixture-admin", s.PublicOrigin, 409},
		{"change_group", strings.Replace(body, `"group_id":17`, `"group_id":99`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"enable_execution", strings.Replace(body, `"enabled":false`, `"enabled":true`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"change_schedule", strings.Replace(body, `"candy_interval_seconds":300`, `"candy_interval_seconds":600`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"change_limits", strings.Replace(body, `"quota":200`, `"quota":400`, 1), "fixture-admin", s.PublicOrigin, 403},
		{"invalid_visibility", strings.Replace(body, `"visibility":"authenticated"`, `"visibility":"public"`, 1), "fixture-admin", s.PublicOrigin, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := s.Intelligence.Store.snapshot()
			r := request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, tc.body, tc.token, tc.origin)
			if r.Code != tc.status || !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
				t.Fatalf("expected unchanged store with status %d, got %d", tc.status, r.Code)
			}
		})
	}
	if native.creates != 1 || native.updates != 0 {
		t.Fatal("rejected change mutated native keys")
	}
	if err := s.Intelligence.Store.change(func(st *intelligenceState) error {
		row := st.Plans[plan.ID]
		row.Plan.OwnerID = 99
		st.Plans[plan.ID] = row
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.Intelligence.Store.snapshot()
	if request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, body, "fixture-admin", s.PublicOrigin).Code != 403 || !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
		t.Fatal("another administrator changed a plan they do not own")
	}
}
