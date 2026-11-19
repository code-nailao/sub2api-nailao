package portal

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func intelligenceSharedPlanBody(revision int64, enabled bool, pelicanSeconds int) string {
	body := strings.Replace(intelligenceInputJSON(revision, enabled), "{", `{"visibility":"authenticated","creative_every_five":false,`, 1)
	if pelicanSeconds == 0 {
		body = strings.Replace(body, `"pelican_interval_seconds":1200`, `"pelican_interval_seconds":0`, 1)
	}
	if revision > 0 {
		body = strings.Replace(body, `"expires_in_days":30`, `"expires_in_days":0`, 1)
	}
	return body
}

func TestIntelligenceUnchangedSharedPlanRefreshesExpiryAndSurvivesStoreReopen(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceSharedPlanBody(0, true, 1200), "fixture-admin", s.PublicOrigin)
	var original IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &original) != nil {
		t.Fatalf("create enabled shared plan: %d %s", r.Code, r.Body.String())
	}
	if !original.Enabled || original.Visibility != "authenticated" || original.ExpiresAt == nil {
		t.Fatal("fixture must have an enabled shared plan with a finite expiry")
	}
	oldExpiry := *original.ExpiresAt
	key := native.keys[original.KeyID]
	key.ExpiresAt = nil
	native.keys[original.KeyID] = key

	// The edit form always sends visibility, even when it has not changed.
	// Only the native expiry changed; no execution or budget edit should be
	// required to refresh the plan's snapshot.
	r = request(s, "PUT", "/api/intelligence-tests/plans/"+original.ID, intelligenceSharedPlanBody(original.Revision, true, 1200), "fixture-admin", s.PublicOrigin)
	var saved IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &saved) != nil {
		t.Fatalf("save unchanged shared plan: %d %s", r.Code, r.Body.String())
	}
	if saved.ExpiresAt != nil || !saved.Enabled || saved.Visibility != "authenticated" || saved.KeyStatus != "active" || saved.Revision != original.Revision+1 {
		t.Fatal("unchanged form did not refresh the native permanent expiry")
	}
	if saved.KeyID != original.KeyID || saved.OwnerID != original.OwnerID || saved.GroupID != original.GroupID || saved.Quota != original.Quota || saved.DailyLimit != original.DailyLimit || native.creates != 1 || native.updates != 0 {
		t.Fatal("refresh changed the native identity or budget")
	}

	// Open a new store from state.json, rather than reusing the old in-memory
	// state. This also checks that its encrypted credential can be recovered.
	reloaded, err := OpenIntelligenceStore(s.Service.Store)
	if err != nil {
		t.Fatal("reopen persisted permanent plan", err)
	}
	reopened, err := NewIntelligenceService(reloaded, s.SubOrigin, testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal("reconstruct service", err)
	}
	now := oldExpiry.Add(365 * 24 * time.Hour)
	fake := &intelligenceFake{}
	reopened.now, reopened.provider = func() time.Time { return now }, fake
	reopened.tick()
	state := reloaded.snapshot()
	plan := state.Plans[original.ID].Plan
	if plan.ExpiresAt != nil || !plan.Enabled || plan.KeyStatus != "active" || fake.submits != 2 || len(state.Runs) != 2 {
		t.Fatal("persisted permanent plan did not schedule and submit after its former expiry")
	}
	for _, run := range state.Runs {
		if run.Run.Status != "running" || run.RemoteID == "" {
			t.Fatal("reopened plan did not preserve an accepted remote task")
		}
	}
	for _, submission := range fake.submissions {
		if submission.APIKey != key.Key || submission.Model != original.Model || submission.Effort != original.Effort {
			t.Fatal("reopened plan submitted with different credentials or model settings")
		}
	}
}

func TestIntelligencePausedSubmittedRunPollsAfterStoreReopenWithoutResubmitting(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	now := time.Now().UTC()
	s.Intelligence.now = func() time.Time { return now }
	fake := &intelligenceFake{}
	s.Intelligence.provider = fake
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceSharedPlanBody(0, true, 0), "fixture-admin", s.PublicOrigin)
	var plan IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &plan) != nil {
		t.Fatalf("create scheduled plan: %d %s", r.Code, r.Body.String())
	}
	s.Intelligence.tick()
	before := s.Intelligence.Store.snapshot()
	if fake.submits != 1 || len(before.Runs) != 1 {
		t.Fatal("fixture must submit exactly one scheduled task")
	}
	var accepted intelligenceRunRecord
	for _, run := range before.Runs {
		accepted = run
	}
	if accepted.Run.Status != "running" || accepted.Run.Trigger != "scheduled" || accepted.RemoteID == "" {
		t.Fatal("fixture must already have an accepted remote task")
	}
	delete(native.keys, plan.KeyID)
	native.groups = nil
	r = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, intelligenceSharedPlanBody(plan.Revision, false, 0), "fixture-admin", s.PublicOrigin)
	var paused IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &paused) != nil || paused.Enabled {
		t.Fatalf("pause unavailable key: %d %s", r.Code, r.Body.String())
	}
	after := s.Intelligence.Store.snapshot()
	if !reflect.DeepEqual(before.Runs, after.Runs) || !reflect.DeepEqual(before.Requests, after.Requests) {
		t.Fatal("pause changed the accepted remote task or its request identity")
	}
	r = request(s, "PUT", "/api/intelligence-tests/plans/"+plan.ID, intelligenceSharedPlanBody(paused.Revision, true, 0), "fixture-admin", s.PublicOrigin)
	if r.Code != 403 || !reflect.DeepEqual(after, s.Intelligence.Store.snapshot()) {
		t.Fatal("pause shortcut allowed the unavailable key to resume", r.Code)
	}

	reloaded, err := OpenIntelligenceStore(s.Service.Store)
	if err != nil {
		t.Fatal("reload paused plan and accepted task", err)
	}
	reopened, err := NewIntelligenceService(reloaded, s.SubOrigin, testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal("reconstruct paused service", err)
	}
	reopened.now, reopened.provider = func() time.Time { return now }, fake
	fake.result.Status, fake.result.Benchmark = "succeeded", "candy"
	fake.result.Candy.Status, fake.result.Candy.Answer = "passed", "21"
	now = now.Add(6 * time.Second)
	reopened.tick()
	completed := reloaded.snapshot().Runs[accepted.Run.ID]
	if completed.RemoteID != accepted.RemoteID || completed.Run.Status != "succeeded" || !completed.Run.HasResult || fake.polls != 1 || fake.submits != 1 {
		t.Fatal("paused task did not finish by polling its original remote ID")
	}
	result, err := reloaded.result(accepted.Run.ID)
	if err != nil || result.Answer != "21" {
		t.Fatal("paused task result was not retained", err)
	}
	now = now.Add(10 * time.Minute)
	reopened.tick()
	if reloaded.snapshot().Plans[plan.ID].Plan.Enabled || fake.submits != 1 || fake.polls != 1 || len(reloaded.snapshot().Runs) != 1 {
		t.Fatal("paused plan resumed automatic work after its accepted task finished")
	}
}
