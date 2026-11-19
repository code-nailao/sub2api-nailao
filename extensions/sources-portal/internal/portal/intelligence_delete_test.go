package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntelligenceDeletePlanPreservesHistoryAndSurvivesRestart(t *testing.T) {
	svc, provider, owner, _ := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, true)
	other := seedIntelligencePlan(t, svc, false)
	run := enqueueIntelligence(t, svc, plan, "candy")
	if err := svc.Store.saveResult(run.ID, IntelligenceResult{Answer: "retained answer"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.change(func(st *intelligenceState) error {
		r := st.Runs[run.ID]
		r.Run.Status, r.Run.HasResult = "succeeded", true
		st.Runs[run.ID] = r
		st.PlanOrder = []string{plan.ID, other.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := svc.Store.snapshot()
	out, err := svc.deletePlan(plan.ID, plan.Revision, 9)
	if err != nil {
		t.Fatal(err)
	}
	after := svc.Store.snapshot()
	if _, ok := after.Plans[plan.ID]; ok || out.ID != plan.ID || out.KeyID != plan.KeyID {
		t.Fatal("deleted plan still present or wrong deletion response")
	}
	if !reflect.DeepEqual(after.Plans[other.ID], before.Plans[other.ID]) || !reflect.DeepEqual(after.Runs, before.Runs) || !reflect.DeepEqual(after.Requests, before.Requests) {
		t.Fatal("deletion changed another plan, history or paid-request deduplication")
	}
	if !reflect.DeepEqual(after.PlanOrder, []string{other.ID}) || after.OrderRevision != before.OrderRevision+1 {
		t.Fatal("deleted plan remained in order or stale sort writes were not invalidated")
	}
	if result, err := svc.Store.result(run.ID); err != nil || result.Answer != "retained answer" {
		t.Fatal("historical result was lost", err)
	}
	if _, _, err := svc.Store.resultView(run.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted plan's results remain visible")
	}
	archivePath := filepath.Join(svc.Store.dir, "plan-archives", out.ArchiveID+".json")
	raw, err := os.ReadFile(archivePath)
	if err != nil || strings.Contains(string(raw), "sk-private-monitor-fixture") {
		t.Fatal("missing archive or plaintext credential archived", err)
	}
	var archive intelligencePlanArchive
	if json.Unmarshal(raw, &archive) != nil || archive.Actor != 9 || !reflect.DeepEqual(archive.Record, before.Plans[plan.ID]) || !reflect.DeepEqual(archive.PlanOrder, before.PlanOrder) {
		t.Fatal("archive cannot recover the original monitoring configuration")
	}
	if info, err := os.Stat(archivePath); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("archive is not private", err)
	}
	if err := svc.Store.reorder(before.OrderRevision, []string{other.ID}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale sort snapshot accepted after deletion")
	}
	store, err := OpenIntelligenceStore(owner)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewIntelligenceService(store, "https://main.example", testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now, restarted.provider = svc.now, provider
	restarted.tick()
	if _, ok := store.snapshot().Plans[plan.ID]; ok || provider.submits != 0 {
		t.Fatal("restart resurrected or dispatched the deleted monitoring plan")
	}
}

func TestIntelligenceDeletePlanBlocksUnfinishedWork(t *testing.T) {
	for _, status := range []string{"queued", "submitting", "running", "unknown"} {
		t.Run(status, func(t *testing.T) {
			svc, _, _, _ := intelligenceFixture(t)
			plan := seedIntelligencePlan(t, svc, false)
			run := enqueueIntelligence(t, svc, plan, "pelican")
			if err := svc.Store.change(func(st *intelligenceState) error {
				r := st.Runs[run.ID]
				r.Run.Status, r.Deadline = status, svc.now().Add(time.Minute)
				st.Runs[run.ID] = r
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := svc.Store.snapshot()
			if _, err := svc.deletePlan(plan.ID, plan.Revision, 9); !errors.Is(err, ErrBusy) {
				t.Fatal("unfinished work was not protected", err)
			}
			if !reflect.DeepEqual(before, svc.Store.snapshot()) {
				t.Fatal("failed deletion changed live state")
			}
		})
	}
	for _, status := range []string{"succeeded", "failed", "unknown"} {
		t.Run("finished_"+status, func(t *testing.T) {
			svc, _, _, _ := intelligenceFixture(t)
			plan := seedIntelligencePlan(t, svc, false)
			run := enqueueIntelligence(t, svc, plan, "candy")
			_ = svc.Store.change(func(st *intelligenceState) error {
				r := st.Runs[run.ID]
				r.Run.Status, r.Deadline = status, svc.now().Add(-time.Second)
				st.Runs[run.ID] = r
				return nil
			})
			if _, err := svc.deletePlan(plan.ID, plan.Revision, 9); err != nil {
				t.Fatal("finished work prevented configuration removal", err)
			}
		})
	}
}

func TestIntelligenceDeletePlanPersistenceFailureKeepsPlan(t *testing.T) {
	for _, failure := range []string{"archive", "state"} {
		t.Run(failure, func(t *testing.T) {
			svc, _, _, _ := intelligenceFixture(t)
			plan := seedIntelligencePlan(t, svc, false)
			before := svc.Store.snapshot()
			writer := svc.Store.writer
			svc.Store.writer = func(path string, value any) error {
				if failure == "state" && path == svc.Store.path || failure == "archive" && filepath.Base(filepath.Dir(path)) == "plan-archives" {
					return errors.New("injected persistence failure")
				}
				return writer(path, value)
			}
			if _, err := svc.deletePlan(plan.ID, plan.Revision, 9); err == nil {
				t.Fatal("failed persistence reported deletion success")
			}
			if !reflect.DeepEqual(before, svc.Store.snapshot()) {
				t.Fatal("failed persistence removed monitoring configuration")
			}
		})
	}
}

func TestIntelligenceDeletePlanHTTPPermissionsAndNativeResources(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	response := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true), "fixture-admin", s.PublicOrigin)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	var plan IntelligencePlan
	_ = json.Unmarshal(response.Body.Bytes(), &plan)
	path, body := "/api/intelligence-tests/plans/"+plan.ID, fmt.Sprintf(`{"revision":%d}`, plan.Revision)
	// Deletion must not invoke any native key or group mutation.
	transport := s.AuthClient.Transport
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("deletion mutated native resources: %s %s", r.Method, r.URL.Path)
		}
		return transport.RoundTrip(r)
	})
	for _, tc := range []struct {
		name, token, origin, body string
		code                      int
	}{
		{"anonymous", "", s.PublicOrigin, body, 401},
		{"ordinary_user", "fixture-user", s.PublicOrigin, body, 403},
		{"foreign_origin", "fixture-admin", "https://other.example", body, 403},
		{"missing_revision", "fixture-admin", s.PublicOrigin, `{}`, 400},
		{"stale_revision", "fixture-admin", s.PublicOrigin, fmt.Sprintf(`{"revision":%d}`, plan.Revision+1), 409},
		{"extra_fields", "fixture-admin", s.PublicOrigin, `{"revision":1,"delete_group":true}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request(s, "DELETE", path, tc.body, tc.token, tc.origin); got.Code != tc.code {
				t.Fatalf("got %d %s", got.Code, got.Body.String())
			}
		})
	}
	key, groups := native.keys[plan.KeyID], append([]StudioGroup(nil), native.groups...)
	if got := request(s, "DELETE", path, body, "fixture-admin", s.PublicOrigin); got.Code != 200 || strings.Contains(got.Body.String(), "encrypted_key") {
		t.Fatalf("delete response: %d %s", got.Code, got.Body.String())
	}
	if !reflect.DeepEqual(native.keys[plan.KeyID], key) || !reflect.DeepEqual(native.groups, groups) || native.creates != 1 || native.updates != 0 {
		t.Fatal("deletion changed native group, key or financial limits")
	}
	if got := request(s, "DELETE", path, body, "fixture-admin", s.PublicOrigin); got.Code != 404 {
		t.Fatal("duplicate delete should report unavailable, never recreate or mutate", got.Code)
	}
	if got := request(s, "GET", "/api/intelligence-tests/settings", "", "fixture-admin", ""); got.Code != 200 || strings.Contains(got.Body.String(), plan.ID) {
		t.Fatal("deleted plan still in settings", got.Body.String())
	}
	if got := request(s, "GET", "/api/intelligence-tests/snapshot", "", "fixture-admin", ""); got.Code != 200 || strings.Contains(got.Body.String(), plan.ID) {
		t.Fatal("deleted plan still visible", got.Body.String())
	}
}

func TestIntelligenceDeletePlanWaitsForKeyReconciliation(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	native.loseCreate = true
	response := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	plans := s.Intelligence.Store.snapshot().Plans
	if response.Code != 503 || len(plans) != 1 || native.creates != 1 {
		t.Fatal("fixture did not create an unresolved native key")
	}
	for _, p := range plans {
		if _, err := s.Intelligence.deletePlan(p.Plan.ID, p.Plan.Revision, 9); !errors.Is(err, ErrIntelligencePending) {
			t.Fatal("unconfirmed native-key creation was discarded", err)
		}
	}
}

func TestIntelligenceDeletePlanAndEnqueueAreAtomic(t *testing.T) {
	for i := 0; i < 24; i++ {
		svc, _, _, _ := intelligenceFixture(t)
		plan := seedIntelligencePlan(t, svc, true)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var enqueueErr, deleteErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, enqueueErr = svc.Enqueue(plan.ID, "candy", "racing-request-001", false)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, deleteErr = svc.deletePlan(plan.ID, plan.Revision, 9)
		}()
		close(start)
		wg.Wait()
		if enqueueErr == nil && !errors.Is(deleteErr, ErrBusy) || deleteErr == nil && !errors.Is(enqueueErr, ErrNotFound) || enqueueErr != nil && deleteErr != nil {
			t.Fatalf("enqueue/delete were not atomic: %v / %v", enqueueErr, deleteErr)
		}
	}
}
