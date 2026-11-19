package processrole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func handoffRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, "http://owner"+path, nil)
}

type countingHandler struct {
	mu    sync.Mutex
	calls int
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func assertHandoffRefusal(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s: status %d, want 503", label, rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != HandoffRetryAfterSeconds {
		t.Fatalf("%s: Retry-After %q", label, got)
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: refusal is not the native JSON envelope: %v (%q)", label, err, rec.Body.String())
	}
	if body.Code != http.StatusServiceUnavailable || body.Reason != "BACKGROUND_HANDOFF_READ_ONLY" ||
		!strings.Contains(body.Message, "read-only") || !strings.Contains(body.Message, "retry") {
		t.Fatalf("%s: unexpected envelope %+v", label, body)
	}
}

func TestDrainingOwnerAnswersAdminReadsOnly(t *testing.T) {
	token := strings.Repeat("d", 40)
	r := New(Config{Role: Background, Token: token})
	r.Ready()
	next := &countingHandler{}
	h := r.Wrap(next)

	before := httptest.NewRecorder()
	h.ServeHTTP(before, handoffRequest(http.MethodPost, "/api/v1/admin/users"))
	if before.Code != http.StatusOK || next.count() != 1 {
		t.Fatalf("live owner refused an admin write: %d", before.Code)
	}

	r.Drain()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodHead, "/api/v1/admin/dashboard/stats"},
		{http.MethodGet, "/.well-known/oauth-protected-resource"},
		{http.MethodGet, "/health"},
		// Stored-data variants of refused upstream reads stay available.
		{http.MethodGet, "/api/v1/admin/accounts/balance-auto?id=7"},
		{http.MethodGet, "/api/v1/admin/accounts/7/usage?source=passive"},
		{http.MethodGet, "/api/v1/admin/accounts/7/ollama-cloud-usage"},
		{http.MethodGet, "/api/v1/admin/accounts/7/stats"},
		{http.MethodGet, "/api/v1/admin/accounts/models"},
		{http.MethodGet, "/api/v1/admin/backups"},
	} {
		calls := next.count()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, handoffRequest(tc.method, tc.path))
		if rec.Code != http.StatusOK || next.count() != calls+1 {
			t.Fatalf("draining owner refused %s %s: %d", tc.method, tc.path, rec.Code)
		}
	}

	upgrade := handoffRequest(http.MethodGet, "/api/v1/admin/ops/ws/qps")
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	refused := []*http.Request{
		handoffRequest(http.MethodPost, "/api/v1/admin/users"),
		handoffRequest(http.MethodPut, "/api/v1/admin/settings"),
		handoffRequest(http.MethodPatch, "/api/v1/admin/accounts/1"),
		handoffRequest(http.MethodDelete, "/api/v1/admin/accounts/1"),
		handoffRequest(http.MethodOptions, "/api/v1/admin/users"),
		// Reviewed DB-only writes are refused here too; serving answers them.
		handoffRequest(http.MethodPut, "/api/v1/admin/channel-monitors/sort-order"),
		handoffRequest(http.MethodPost, "/api/v1/mcp"),
		handoffRequest(http.MethodGet, "/api/v1/mcp"),
		handoffRequest(http.MethodGet, "/api/v1/admin/sources/image-tasks"),
		handoffRequest(http.MethodGet, "/api/v1/admin/accounts/balance-auto?id=7&refresh=1"),
		handoffRequest(http.MethodGet, "/api/v1/admin/accounts/7/usage"),
		handoffRequest(http.MethodGet, "/api/v1/admin/accounts/7/usage?source=active&force=true"),
		handoffRequest(http.MethodGet, "/api/v1/admin/accounts/7/claude/reset-credits"),
		handoffRequest(http.MethodGet, "/api/v1/admin/accounts/7/models"),
		handoffRequest(http.MethodGet, "/api/v1/admin/openai/accounts/7/quota"),
		handoffRequest(http.MethodHead, "/api/v1/admin/grok/accounts/7/quota"),
		handoffRequest(http.MethodGet, "/api/v1/admin/cn-providers/accounts/7/quota"),
		handoffRequest(http.MethodGet, "/api/v1/admin/cn-providers/accounts/7/balance"),
		upgrade,
	}
	for _, req := range refused {
		calls := next.count()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertHandoffRefusal(t, rec, req.Method+" "+req.URL.Path)
		if next.count() != calls {
			t.Fatalf("%s %s reached the handler during the handoff", req.Method, req.URL.Path)
		}
	}

	// Public paths keep the existing role boundary.
	public := httptest.NewRecorder()
	h.ServeHTTP(public, handoffRequest(http.MethodGet, "/v1/models"))
	if public.Code != http.StatusServiceUnavailable || !strings.Contains(public.Body.String(), "serving instance") {
		t.Fatalf("public boundary changed: %d %q", public.Code, public.Body.String())
	}

	// The release tool still reads the runtime while the owner drains.
	probe := handoffRequest(http.MethodGet, "/internal/sources/runtime")
	probe.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, probe)
	var status Status
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil {
		t.Fatalf("runtime probe during drain: %d %q", rec.Code, rec.Body.String())
	}
	if status.Ready || !status.Draining || status.AdminDuringDrain != AdminDuringDrainReadOnly || status.Role != Background {
		t.Fatalf("draining owner status: %+v", status)
	}
}

func TestServingAndStandaloneKeepTheirAdminBehaviorWhileDraining(t *testing.T) {
	serving := New(Config{Role: Serving})
	servingNext := &countingHandler{}
	sh := serving.Wrap(servingNext)
	serving.Drain()
	rec := httptest.NewRecorder()
	sh.ServeHTTP(rec, handoffRequest(http.MethodPut, "/api/v1/admin/channel-monitors/sort-order"))
	if rec.Code != http.StatusOK || servingNext.count() != 1 {
		t.Fatalf("draining serving refused a reviewed admin write: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	sh.ServeHTTP(rec, handoffRequest(http.MethodGet, "/api/v1/admin/users"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "background owner") {
		t.Fatalf("serving admin boundary changed: %d %q", rec.Code, rec.Body.String())
	}

	standalone := New(Config{Role: Standalone})
	standaloneNext := &countingHandler{}
	ah := standalone.Wrap(standaloneNext)
	standalone.Drain()
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec := httptest.NewRecorder()
		ah.ServeHTTP(rec, handoffRequest(method, "/api/v1/admin/users"))
		if rec.Code != http.StatusOK {
			t.Fatalf("draining standalone refused %s: %d", method, rec.Code)
		}
	}
	for _, status := range []Status{serving.Status(), standalone.Status()} {
		if status.AdminDuringDrain != "" || status.Stopping != nil || status.ActiveAdminWrites != 0 {
			t.Fatalf("handoff fields on a non-owner role: %+v", status)
		}
	}
}

func TestWaitAdminWritesCoversWritesAdmittedBeforeDrain(t *testing.T) {
	r := New(Config{Role: Background})
	type inFlight struct {
		label string
		req   *http.Request
	}
	upgrade := handoffRequest(http.MethodGet, "/api/v1/admin/ops/ws/qps")
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	requests := []inFlight{
		{"write", handoffRequest(http.MethodPost, "/api/v1/admin/users")},
		// A read the draining owner refuses: it probes upstream and writes
		// account state, so it must finish before any service stops.
		{"probe", handoffRequest(http.MethodGet, "/api/v1/admin/accounts/7/usage")},
		{"read", handoffRequest(http.MethodGet, "/api/v1/admin/users")},
		// Upgraded connections are closed later; counting them would wait for
		// the ops realtime page forever.
		{"upgrade", upgrade},
	}
	entered := make(chan string, len(requests))
	release := map[string]chan struct{}{}
	for _, rq := range requests {
		release[rq.label] = make(chan struct{})
	}
	h := r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		label := req.Header.Get("X-Label")
		entered <- label
		if label == "upgrade" {
			<-req.Context().Done()
			return
		}
		<-release[label]
		w.WriteHeader(http.StatusOK)
	}))
	var served sync.WaitGroup
	for _, rq := range requests {
		rq.req.Header.Set("X-Label", rq.label)
		served.Add(1)
		go func(req *http.Request) {
			defer served.Done()
			h.ServeHTTP(httptest.NewRecorder(), req)
		}(rq.req)
		<-entered
	}
	if got := r.Status().ActiveAdminWrites; got != 2 {
		t.Fatalf("counted in-flight admin requests = %d, want 2 (the write and the refused read)", got)
	}
	r.Drain()
	waited := make(chan error, 1)
	go func() { waited <- r.WaitAdminWrites(context.Background()) }()
	for _, label := range []string{"read", "write"} {
		close(release[label])
		select {
		case err := <-waited:
			t.Fatalf("returned after %q while a refused read admitted before Drain was in flight: %v", label, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	close(release["probe"])
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not return after the last counted request finished (the open upgrade must not count)")
	}
	r.CloseAdminUpgrades()
	served.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := New(Config{Role: Background}).WaitAdminWrites(ctx); err != nil {
		t.Fatalf("idle owner waited: %v", err)
	}
}

func TestCloseAdminUpgradesCancelsOnlyUpgradedRequests(t *testing.T) {
	r := New(Config{Role: Background})
	type observed struct {
		upgrade bool
		err     error
	}
	results := make(chan observed, 2)
	entered := make(chan struct{}, 2)
	releaseNormal := make(chan struct{})
	h := r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		if req.Header.Get("Upgrade") != "" {
			<-req.Context().Done()
			results <- observed{true, req.Context().Err()}
			return
		}
		<-releaseNormal
		results <- observed{false, req.Context().Err()}
	}))
	upgrade := handoffRequest(http.MethodGet, "/api/v1/admin/ops/ws/qps")
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	go h.ServeHTTP(httptest.NewRecorder(), upgrade)
	go h.ServeHTTP(httptest.NewRecorder(), handoffRequest(http.MethodGet, "/api/v1/admin/users"))
	<-entered
	<-entered
	r.Drain()
	r.CloseAdminUpgrades()
	select {
	case got := <-results:
		if !got.upgrade || got.err == nil {
			t.Fatalf("first to finish should be the cancelled upgrade: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upgraded request was not cancelled")
	}
	close(releaseNormal)
	select {
	case got := <-results:
		if got.upgrade || got.err != nil {
			t.Fatalf("ordinary request context was touched: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary request did not finish")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	left := len(r.upgrades)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d upgrade registrations leaked", left)
	}
}

func TestStoppingStepsAreReportedUntilDone(t *testing.T) {
	r := New(Config{Role: Background})
	previous := current
	current = r
	t.Cleanup(func() { current = previous })
	doneBackup := Stopping("BackupService")
	doneOAuth := Stopping("TokenRefreshService")
	if got := r.Status().Stopping; len(got) != 2 || got[0] != "BackupService" || got[1] != "TokenRefreshService" {
		t.Fatalf("stopping = %v", got)
	}
	doneOAuth()
	doneOAuth() // Idempotent.
	if got := r.Status().Stopping; len(got) != 1 || got[0] != "BackupService" {
		t.Fatalf("stopping = %v", got)
	}
	doneBackup()
	if got := r.Status().Stopping; got != nil {
		t.Fatalf("stopping after all steps finished = %v", got)
	}
}
