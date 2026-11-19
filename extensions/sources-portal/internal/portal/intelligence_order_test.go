package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func intelligenceOrderFixture(t *testing.T) (*Server, []string) {
	t.Helper()
	s, _ := intelligenceAPIFixture(t)
	ids := []string{fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2), fmt.Sprintf("%032x", 3)}
	err := s.Intelligence.Store.change(func(st *intelligenceState) error {
		st.ViewerPolicy = &intelligenceViewerPolicy{Version: 1, Access: "authenticated"}
		for i, id := range ids {
			visibility := "authenticated"
			if i == 1 {
				visibility = "admin"
			}
			expires := time.Now().Add(-time.Hour)
			plan := IntelligencePlan{ID: id, GroupID: int64(17 + i), OwnerID: 9, Revision: 7, Name: fmt.Sprintf("Group %d", i+1), Visibility: visibility, Model: intelligenceModel, Effort: "medium", KeyID: int64(100 + i), KeyStatus: "expired", Quota: 200, DailyLimit: 20, ExpiresAt: &expires, CandySeconds: 300, PelicanSeconds: 1200, CreatedAt: time.Unix(int64(i), 0).UTC(), NextCandy: time.Now().Add(time.Hour), DrawingSequence: 4}
			secret, err := s.Intelligence.Store.encrypt(plan, "sk-private-monitor-fixture")
			if err != nil {
				return err
			}
			st.Plans[id] = intelligencePlanRecord{Plan: plan, Secret: secret, Operation: "pending-key-operation"}
			st.Runs[id] = intelligenceRunRecord{Run: IntelligenceRun{ID: id, PlanID: id, Status: "running", Benchmark: "candy"}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, ids
}

func orderBody(revision int64, ids []string) string {
	raw, _ := json.Marshal(map[string]any{"revision": revision, "plan_ids": ids})
	return string(raw)
}

func responsePlanIDs(t *testing.T, s *Server, path, token string) []string {
	t.Helper()
	r := request(s, "GET", path, "", token, "")
	var out struct {
		Plans []struct{ ID string }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
		t.Fatalf("read plans %s: %d %s", path, r.Code, r.Body.String())
	}
	ids := []string{}
	for _, plan := range out.Plans {
		ids = append(ids, plan.ID)
	}
	return ids
}

func TestIntelligenceOrderPersistsWithoutChangingExecutionOrVisibility(t *testing.T) {
	s, ids := intelligenceOrderFixture(t)
	before := s.Intelligence.Store.snapshot()
	// Reordering must not depend on available groups or usable native keys.
	originalTransport := s.AuthClient.Transport
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/auth/me" {
			t.Fatalf("ordering unexpectedly called native execution/config API: %s", r.URL.Path)
		}
		return originalTransport.RoundTrip(r)
	})
	want := []string{ids[2], ids[1], ids[0]}
	for _, path := range []string{"/api/intelligence-tests/order", "/api/intelligence-tests/snapshot"} {
		if got := responsePlanIDs(t, s, path, "fixture-admin"); !reflect.DeepEqual(got, ids) {
			t.Fatalf("legacy order = %v", got)
		}
	}
	r := request(s, "PUT", "/api/intelligence-tests/order", orderBody(0, want), "fixture-admin", s.PublicOrigin)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	after := s.Intelligence.Store.snapshot()
	if !reflect.DeepEqual(before.Plans, after.Plans) || !reflect.DeepEqual(before.Runs, after.Runs) ||
		!reflect.DeepEqual(before.Requests, after.Requests) || before.SettingsRevision != after.SettingsRevision ||
		!reflect.DeepEqual(before.ViewerPolicy, after.ViewerPolicy) {
		t.Fatal("display ordering changed plans, credentials, schedules, history or visibility")
	}
	if after.OrderRevision != 1 || !reflect.DeepEqual(after.PlanOrder, want) {
		t.Fatal("order not committed")
	}
	reopened, err := OpenIntelligenceStore(s.Service.Store)
	if err != nil {
		t.Fatal(err)
	}
	s.Intelligence.Store = reopened
	if got := responsePlanIDs(t, s, "/api/intelligence-tests/snapshot", "fixture-admin"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened order = %v", got)
	}
	if got := responsePlanIDs(t, s, "/api/intelligence-tests/snapshot", "fixture-user"); !reflect.DeepEqual(got, []string{ids[2], ids[0]}) {
		t.Fatalf("member order/private visibility = %v", got)
	}
	s.AuthClient.Transport = originalTransport
	if got := responsePlanIDs(t, s, "/api/intelligence-tests/settings", "fixture-admin"); !reflect.DeepEqual(got, want) {
		t.Fatal("settings order differs from viewer order", got)
	}
}

func TestIntelligenceOrderRejectsUnauthorizedAndInvalidChanges(t *testing.T) {
	s, ids := intelligenceOrderFixture(t)
	before := s.Intelligence.Store.snapshot()
	for _, tc := range []struct {
		name, method, token, origin, body string
		status                            int
	}{
		{"anonymous read", "GET", "", "", "", 401},
		{"anonymous write", "PUT", "", s.PublicOrigin, orderBody(0, ids), 401},
		{"member read", "GET", "fixture-user", "", "", 403},
		{"member write", "PUT", "fixture-user", s.PublicOrigin, orderBody(0, ids), 403},
		{"wrong origin", "PUT", "fixture-admin", "https://evil.example", orderBody(0, ids), 403},
		{"stale revision", "PUT", "fixture-admin", s.PublicOrigin, orderBody(1, ids), 409},
		{"negative revision", "PUT", "fixture-admin", s.PublicOrigin, orderBody(-1, ids), 400},
		{"missing plan", "PUT", "fixture-admin", s.PublicOrigin, orderBody(0, ids[:2]), 409},
		{"duplicate plan", "PUT", "fixture-admin", s.PublicOrigin, orderBody(0, []string{ids[0], ids[0], ids[2]}), 400},
		{"unknown plan", "PUT", "fixture-admin", s.PublicOrigin, orderBody(0, []string{ids[0], ids[1], strings.Repeat("f", 32)}), 400},
		{"missing ids", "PUT", "fixture-admin", s.PublicOrigin, `{"revision":0}`, 400},
		{"null ids", "PUT", "fixture-admin", s.PublicOrigin, `{"revision":0,"plan_ids":null}`, 400},
		{"extra settings", "PUT", "fixture-admin", s.PublicOrigin, strings.TrimSuffix(orderBody(0, ids), "}") + `,"enabled":true}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(s, tc.method, "/api/intelligence-tests/order?manage=1&role=admin", tc.body, tc.token, tc.origin)
			if r.Code != tc.status {
				t.Fatalf("status = %d: %s", r.Code, r.Body.String())
			}
			if !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
				t.Fatal("rejected change mutated storage")
			}
		})
	}
}

func TestIntelligenceOrderNewPlanAppendsAndStaleListConflicts(t *testing.T) {
	s, ids := intelligenceOrderFixture(t)
	want := []string{ids[2], ids[0], ids[1]}
	if err := s.Intelligence.Store.reorder(0, want); err != nil {
		t.Fatal(err)
	}
	fresh := seedIntelligencePlan(t, s.Intelligence, false)
	got := responsePlanIDs(t, s, "/api/intelligence-tests/order", "fixture-admin")
	if !reflect.DeepEqual(got, append(append([]string{}, want...), fresh.ID)) {
		t.Fatalf("new plan displaced saved order: %v", got)
	}
	if r := request(s, "PUT", "/api/intelligence-tests/order", orderBody(1, want), "fixture-admin", s.PublicOrigin); r.Code != 409 {
		t.Fatal("outdated plan list overwrote a new plan")
	}
}

func TestIntelligenceOrderConcurrentSavesDoNotOverwriteEachOther(t *testing.T) {
	s, ids := intelligenceOrderFixture(t)
	var workers sync.WaitGroup
	errorsOut := make(chan error, 2)
	for _, order := range [][]string{ids, {ids[2], ids[1], ids[0]}} {
		workers.Add(1)
		go func(order []string) {
			defer workers.Done()
			errorsOut <- s.Intelligence.Store.reorder(0, order)
		}(order)
	}
	workers.Wait()
	close(errorsOut)
	succeeded, conflicted := 0, 0
	for err := range errorsOut {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrConflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 || s.Intelligence.Store.snapshot().OrderRevision != 1 {
		t.Fatal("concurrent administrator silently overwrote a saved order")
	}
}

func TestIntelligenceOrderPersistenceFailureIsNotReportedAsSaved(t *testing.T) {
	s, ids := intelligenceOrderFixture(t)
	before := s.Intelligence.Store.snapshot()
	s.Intelligence.Store.writer = func(string, any) error { return errors.New("disk unavailable") }
	r := request(s, "PUT", "/api/intelligence-tests/order", orderBody(0, []string{ids[2], ids[1], ids[0]}), "fixture-admin", s.PublicOrigin)
	if r.Code != 503 || !reflect.DeepEqual(before, s.Intelligence.Store.snapshot()) {
		t.Fatal("failed persistence was published as successful ordering")
	}
}
