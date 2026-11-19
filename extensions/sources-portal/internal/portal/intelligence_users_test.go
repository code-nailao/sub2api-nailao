package portal

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestIntelligenceMemberReadHasNoManagementOrCredentialAccess(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", strings.Replace(intelligenceInputJSON(0, false), "{", `{"visibility":"authenticated",`, 1), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
		t.Fatal("plan fixture")
	}
	run, err := s.Intelligence.Enqueue(p.ID, "candy", "viewer-contract-request", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Intelligence.Store.saveResult(run.ID, IntelligenceResult{Answer: "21"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		v := st.Runs[run.ID]
		v.Run.HasResult = true
		v.RemoteID = "private-task-id"
		st.Runs[run.ID] = v
		return nil
	})
	if request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":0,"admin_only":false}`, "fixture-admin", s.PublicOrigin).Code != 400 {
		t.Fatal("legacy public flag accepted")
	}
	if request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":0,"viewer_access":"authenticated"}`, "fixture-admin", s.PublicOrigin).Code != 200 {
		t.Fatal("open member access")
	}
	if !s.Intelligence.Store.snapshot().AdminOnly {
		t.Fatal("041 rollback would become anonymous")
	}
	for _, path := range []string{"/api/intelligence-tests/snapshot", "/api/intelligence-tests/runs/" + run.ID} {
		res := request(s, "GET", path+"?manage=1&role=admin", "", "fixture-user", "")
		if res.Code != 200 {
			t.Fatal(path, res.Code)
		}
		for _, secret := range []string{"sk-private", "encrypted_key", "key_id", "owner_id", "private-task-id", "key_status", "group_id"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatal("viewer leaked", secret)
			}
		}
		if request(s, "GET", path, "", "", "").Code != 401 {
			t.Fatal("anonymous read")
		}
	}
	for _, test := range []struct{ method, path, body string }{
		{"GET", "/api/intelligence-tests/settings", ""},
		{"PUT", "/api/intelligence-tests/settings", `{"revision":1,"viewer_access":"admin"}`},
		{"POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, true)},
		{"PUT", "/api/intelligence-tests/plans/" + p.ID, intelligenceInputJSON(p.Revision, true)},
		{"POST", "/api/intelligence-tests/plans/" + p.ID + "/run", `{"benchmark":"creative","request_id":"not-permitted-paid-call"}`},
		{"POST", "/api/intelligence-tests/plans/" + p.ID + "/clear-history", fmt.Sprintf(`{"revision":%d,"run_ids":[%q]}`, p.Revision, run.ID)},
	} {
		if res := request(s, test.method, test.path, test.body, "fixture-user", s.PublicOrigin); res.Code != 403 {
			t.Fatal("management widened", test.method, test.path, res.Code)
		}
	}
	for _, token := range []string{"", "fixture-user", "fixture-admin"} {
		if request(s, "GET", "/public/intelligence-tests", "", token, "").Code != 401 {
			t.Fatal("public cache namespace exposed")
		}
	}
	if native.creates != 1 || native.updates != 0 {
		t.Fatal("read issued native writes")
	}
	if request(s, "PUT", "/api/intelligence-tests/settings", `{"revision":1,"viewer_access":"admin"}`, "fixture-admin", s.PublicOrigin).Code != 200 || request(s, "GET", "/api/intelligence-tests/snapshot", "", "fixture-user", "").Code != 403 {
		t.Fatal("closing access ineffective")
	}
}

func TestIntelligenceSecretsAreBlockedAtSaveAndLegacyRead(t *testing.T) {
	s, _ := intelligenceAPIFixture(t)
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	run, _ := s.Intelligence.Enqueue(p.ID, "creative", "secret-guard-fixture", true)
	for _, body := range []string{"sk-private-native-key", "Bearer a-private-admin-token-123456", base64.StdEncoding.EncodeToString([]byte("sk-private-native-key")), "&#115;&#107;-private-native-key", "%73%6b-private-native-key"} {
		if s.Intelligence.Store.saveResult(run.ID, IntelligenceResult{HTML: "<div>" + body + "</div>"}) != ErrUnsafeIntelligenceResult {
			t.Fatal("secret accepted")
		}
	}
	// Simulate a pre-upgrade result; reading it must not release its raw contents.
	if err := writeJSONAtomic(filepath.Join(s.Intelligence.Store.dir, "results", run.ID+".json"), IntelligenceResult{HTML: "sk-private-native-key"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		r := st.Runs[run.ID]
		r.Run.HasResult = true
		st.Runs[run.ID] = r
		return nil
	})
	r := request(s, "GET", "/api/intelligence-tests/runs/"+run.ID, "", "fixture-admin", "")
	if r.Code != 404 || strings.Contains(r.Body.String(), "sk-private") {
		t.Fatal("legacy credential returned")
	}
}

func TestIntelligenceHistoryClearKeepsLiveWorkAndPaidTombstones(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(r.Body.Bytes(), &p)
	done, _ := s.Intelligence.Enqueue(p.ID, "candy", "completed-old-request", true)
	_ = s.Intelligence.Store.saveResult(done.ID, IntelligenceResult{Answer: "21"})
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		v := st.Runs[done.ID]
		v.Run.Status = "succeeded"
		v.Run.HasResult = true
		st.Runs[done.ID] = v
		return nil
	})
	live, _ := s.Intelligence.Enqueue(p.ID, "creative", "existing-active-request", true)
	newer, _ := s.Intelligence.Enqueue(p.ID, "candy", "new-accepted-request", true)
	before := s.Intelligence.Store.snapshot()
	hash := before.Runs[done.ID].RequestHash
	body := fmt.Sprintf(`{"revision":%d,"run_ids":[%q,%q]}`, p.Revision, done.ID, live.ID)
	res := request(s, "POST", "/api/intelligence-tests/plans/"+p.ID+"/clear-history", body, "fixture-admin", s.PublicOrigin)
	var out intelligenceHistoryClear
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &out) != nil || len(out.RemovedIDs) != 1 || out.SkippedActive != 1 {
		t.Fatal(res.Code, res.Body.String())
	}
	st := s.Intelligence.Store.snapshot()
	if _, ok := st.Runs[done.ID]; ok {
		t.Fatal("old row survived")
	}
	if st.Runs[live.ID].Run.ID == "" || st.Runs[newer.ID].Run.ID == "" || st.Requests[hash].RunID != done.ID {
		t.Fatal("live work or dedupe removed")
	}
	if _, err := os.Stat(filepath.Join(s.Intelligence.Store.dir, "results", done.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("result file retained")
	}
	info, err := os.Stat(filepath.Join(s.Intelligence.Store.dir, "history-archives", out.ArchiveID+".json"))
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private backup missing")
	}
	if request(s, "POST", "/api/intelligence-tests/plans/"+p.ID+"/clear-history", body, "fixture-admin", s.PublicOrigin).Code != 409 {
		t.Fatal("stale clear replayed")
	}
	if _, err = s.Intelligence.Enqueue(p.ID, "candy", "completed-old-request", true); err != ErrConflict {
		t.Fatal("paid request replayed after clear", err)
	}
	after := st.Plans[p.ID].Plan
	expected := before.Plans[p.ID].Plan
	expected.Revision++
	a, _ := json.Marshal(after)
	b, _ := json.Marshal(expected)
	if string(a) != string(b) || native.creates != 1 || native.updates != 0 {
		t.Fatal("clear changed configuration or native key")
	}
	if request(s, "GET", "/api/intelligence-tests/runs/"+done.ID, "", "fixture-admin", "").Code != 404 {
		t.Fatal("cleared result still reachable")
	}
}
func TestIntelligenceClearBackupFailureDoesNotRemoveRows(t *testing.T) {
	s, _ := intelligenceAPIFixture(t)
	r := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(r.Body.Bytes(), &p)
	run, _ := s.Intelligence.Enqueue(p.ID, "candy", "backup-failure-operation", true)
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		v := st.Runs[run.ID]
		v.Run.Status = "failed"
		st.Runs[run.ID] = v
		return nil
	})
	_ = os.WriteFile(filepath.Join(s.Intelligence.Store.dir, "history-archives"), []byte("blocked"), 0600)
	if _, err := s.Intelligence.clearHistory(p.ID, p.Revision, []string{run.ID}, 9); err == nil {
		t.Fatal("backup failure ignored")
	}
	if s.Intelligence.Store.snapshot().Runs[run.ID].Run.ID == "" {
		t.Fatal("deleted without backup")
	}
}
func TestPortalHashedStaticAssetsOnlyAreCacheable(t *testing.T) {
	s, _ := intelligenceAPIFixture(t)
	s.HomeFiles = fstest.MapFS{"assets/test-12345678.js": &fstest.MapFile{Data: []byte("console.log('public code')"), Mode: fs.FileMode(0644), ModTime: time.Now()}}
	for _, tc := range []struct {
		path  string
		cache bool
	}{{"/home-assets/assets/test-12345678.js", true}, {"/home-assets/assets/missing-12345678.js", false}, {"/api/intelligence-tests/snapshot", false}, {"/public/intelligence-tests", false}} {
		res := request(s, http.MethodGet, tc.path, "", "fixture-admin", "")
		if strings.Contains(res.Header().Get("Cache-Control"), "immutable") != tc.cache {
			t.Fatal("wrong cache boundary", tc.path)
		}
	}
}

func TestIntelligenceViewerOrderIsStableForEqualTimestamps(t *testing.T) {
	s := intelligenceBrowserFixture(t)
	_, _, first, _ := s.Intelligence.Store.viewerSnapshot(true)
	want, _ := json.Marshal(first)
	for i := 0; i < 10; i++ {
		_, _, rows, err := s.Intelligence.Store.viewerSnapshot(true)
		got, _ := json.Marshal(rows)
		if err != nil || string(got) != string(want) {
			t.Fatal("unchanged snapshot order changed")
		}
	}
}
func TestIntelligenceClearSkipsUncertainLiveAndRejectsWrongGroup(t *testing.T) {
	s := intelligenceBrowserFixture(t)
	st := s.Intelligence.Store.snapshot()
	var p string
	var own, other string
	for id := range st.Plans {
		if p == "" {
			p = id
		}
	}
	for id, r := range st.Runs {
		if r.Run.PlanID == p {
			own = id
		} else {
			other = id
		}
	}
	_ = s.Intelligence.Store.change(func(st *intelligenceState) error {
		r := st.Runs[own]
		r.Run.Status = "unknown"
		r.Deadline = s.Intelligence.now().Add(time.Hour)
		st.Runs[own] = r
		return nil
	})
	before := s.Intelligence.Store.snapshot()
	if _, err := s.Intelligence.clearHistory(p, before.Plans[p].Plan.Revision, []string{own, other}, 9); err != ErrIntelligenceInvalid {
		t.Fatal("cross-group selection accepted", err)
	}
	got, err := s.Intelligence.clearHistory(p, before.Plans[p].Plan.Revision, []string{own}, 9)
	if err != nil || len(got.RemovedIDs) != 0 || got.SkippedActive != 1 || s.Intelligence.Store.snapshot().Runs[own].Run.ID == "" {
		t.Fatal("uncertain work removed", err)
	}
}
