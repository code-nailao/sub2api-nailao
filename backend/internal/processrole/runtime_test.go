package processrole

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSplitOwnerExcludesSecondProcessGeneration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	blue := New(Config{Role: Background, LockDir: dir})
	if err := blue.acquireOwner(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blue.closeOwner() })
	green := New(Config{Role: Background, LockDir: dir})
	if err := green.acquireOwner(); err == nil {
		t.Fatal("second background acquired a live owner's lock")
	}
	blue.Drain()
	if err := green.acquireOwner(); err == nil {
		t.Fatal("draining must not release ownership")
	}
	serving := New(Config{Role: Serving, LockDir: dir})
	if err := serving.acquireOwner(); err != nil || serving.owner != nil {
		t.Fatal("serving must coexist without taking ownership")
	}
	if err := blue.closeOwner(); err != nil {
		t.Fatal(err)
	}
	if err := green.acquireOwner(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = green.closeOwner() })
	if !green.Status().OwnsBackground {
		t.Fatal("new owner missing lock evidence")
	}
}

func TestOwnerLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other-lock")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "native-background.lock")); err != nil {
		t.Fatal(err)
	}
	if err := New(Config{Role: Background, LockDir: dir}).acquireOwner(); err == nil {
		t.Fatal("symlink lock accepted")
	}
}

func TestSplitConfigFailsBeforeNativeStartup(t *testing.T) {
	t.Setenv("SOURCES_PROCESS_ROLE", "serving")
	t.Setenv("SOURCES_RUNTIME_TOKEN_FILE", "")
	t.Setenv("SOURCES_BACKGROUND_LOCK_DIR", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("split startup accepted missing ownership/control configuration")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 48)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCES_RUNTIME_TOKEN_FILE", path)
	t.Setenv("SOURCES_BACKGROUND_LOCK_DIR", dir)
	if _, err := ConfigFromEnv(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("world-readable control secret accepted")
	}
	t.Setenv("SOURCES_PROCESS_ROLE", "serveing")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("misspelled role silently started full native background")
	}
}

func TestDrainKeepsExistingStreamAndWaitsForAcceptedImages(t *testing.T) {
	r := New(Config{Role: Serving})
	r.Ready()
	continueStream := make(chan struct{})
	server := httptest.NewServer(r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		<-continueStream
		_, _ = io.WriteString(w, "last\n")
	})))
	t.Cleanup(server.Close)
	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	reader := bufio.NewReader(resp.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("first chunk: %q %v", first, err)
	}
	finishImage := r.BeginImage()
	r.Drain()
	r.Ready() // Cannot accidentally re-advertise the same generation.
	if r.Status().Ready {
		t.Fatal("draining generation became ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if r.Wait(ctx) == nil {
		t.Fatal("active stream/image was considered drained")
	}
	close(continueStream)
	if tail, err := io.ReadAll(reader); err != nil || string(tail) != "last\n" {
		t.Fatalf("stream interrupted: %q %v", tail, err)
	}
	if r.Status().ActiveImages != 1 {
		t.Fatal("image disappeared when HTTP ended")
	}
	finishImage(true)
	finishImage(true) // Completion is idempotent.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := r.Wait(ctx2); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownImagePersistenceBlocksRetirement(t *testing.T) {
	r := New(Config{Role: Serving})
	r.BeginImage()(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := r.Wait(ctx); err == nil {
		t.Fatal("lost image completion allowed process retirement")
	}
	if got := r.Status(); got.UnresolvedImages != 1 || got.ActiveImages != 0 {
		t.Fatalf("unexpected drain status: %+v", got)
	}
}

func TestHijackedConnectionRemainsTrackedAfterHandlerReturns(t *testing.T) {
	r := New(Config{Role: Serving})
	connReady := make(chan net.Conn, 1)
	server := httptest.NewServer(r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\n")
		_ = rw.Flush()
		connReady <- conn
	})))
	t.Cleanup(server.Close)
	client, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, _ = io.WriteString(client, "GET / HTTP/1.1\r\nHost: fixture\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\n")
	serverConn := <-connReady
	t.Cleanup(func() { _ = serverConn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if r.Wait(ctx) == nil || r.Status().Hijacked != 1 {
		t.Fatal("upgraded connection vanished after handler return")
	}
	_ = serverConn.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := r.Wait(ctx2); err != nil {
		t.Fatal(err)
	}
}

func TestControlDoesNotCountProbesOrExposePublicSwitch(t *testing.T) {
	r := New(Config{Role: Serving, Token: strings.Repeat("c", 40)})
	r.Ready()
	h := r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(method, path, token string) *httptest.ResponseRecorder {
		var body io.Reader
		if path == "/internal/sources/drain" {
			body = strings.NewReader(`{"instance_id":"` + r.id + `"}`)
		}
		req := httptest.NewRequest(method, path, body)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := call("POST", "/internal/sources/drain", ""); got.Code != 404 || r.Status().Draining {
		t.Fatal("unauthenticated drain accepted")
	}
	w := call("GET", "/internal/sources/runtime", r.config.Token)
	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ActiveHTTP != 0 || !got.Ready {
		t.Fatal("operator probes interfere with drain")
	}
	if w := call("POST", "/api/v1/admin/backups", ""); w.Code != 503 {
		t.Fatal("serving allowed manual background operation")
	}
	if w := call("POST", "/api/v1/payments/notify", ""); w.Code != 204 {
		t.Fatal("payment notification was disabled along with background")
	}
	if w := call("POST", "/internal/sources/drain", r.config.Token); w.Code != 200 || !r.Status().Draining {
		t.Fatal("authorized drain failed")
	}
	if w := call("GET", "/v1/models", ""); w.Code != 204 {
		t.Fatal("drain interrupted old nginx connections")
	}
}

func TestServingMonitorDisplayKeepsAuthenticationAndBackgroundBoundary(t *testing.T) {
	cases := []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/api/v1/admin/channel-monitors?page=2", true},
		{"GET", "/api/v1/admin/accounts/balance-identities?ids=1,2", true},
		{"GET", "/api/v1/admin/accounts/balance-auto?id=1", true},
		{"POST", "/api/v1/admin/accounts/balance-auto", false},
		{"POST", "/api/v1/admin/accounts/wallet-source-deletion", true},
		{"GET", "/api/v1/admin/accounts/wallet-source-deletion", false},
		{"PUT", "/api/v1/admin/accounts/wallet-source-deletion", false},
		{"DELETE", "/api/v1/admin/accounts/wallet-source-deletion", false},
		{"POST", "/api/v1/admin/accounts/wallet-source-deletion/", false},
		{"POST", "/api/v1/admin/accounts/wallet-source-deletion/another", false},
		{"GET", "/api/v1/admin/accounts/balance-config?id=1", true},
		{"PUT", "/api/v1/admin/accounts/balance-config?id=1", true},
		{"POST", "/api/v1/admin/accounts/balance-config/test?id=1", true},
		{"POST", "/api/v1/admin/accounts/balance-config", false},
		{"DELETE", "/api/v1/admin/accounts/balance-config", false},
		{"GET", "/api/v1/admin/accounts/balance-config/test", false},
		{"PUT", "/api/v1/admin/accounts/balance-config/", false},
		{"GET", "/api/v1/admin/accounts/balance-auto/", false},
		{"POST", "/api/v1/admin/accounts/balance-identities", false},
		{"GET", "/api/v1/admin/accounts/balance-identities/", false},
		{"GET", "/api/v1/admin/accounts/1", false},
		{"GET", "/api/v1/admin/channel-monitors/1", true},
		{"GET", "/api/v1/admin/channel-monitors/9223372036854775807", true},
		{"PUT", "/api/v1/admin/channel-monitors/sort-order", true},
		{"POST", "/api/v1/admin/channel-monitors", false},
		{"PUT", "/api/v1/admin/channel-monitors/1", false},
		{"DELETE", "/api/v1/admin/channel-monitors/1", false},
		{"POST", "/api/v1/admin/channel-monitors/1/duplicate", false},
		{"POST", "/api/v1/admin/channel-monitors/1/run", false},
		{"GET", "/api/v1/admin/channel-monitors/1/history", false},
		{"GET", "/api/v1/admin/channel-monitors/sort-order", false},
		{"PATCH", "/api/v1/admin/channel-monitors/sort-order", false},
		{"DELETE", "/api/v1/admin/channel-monitors/sort-order", false},
		{"PUT", "/api/v1/admin/channel-monitors/sort-order/", false},
		{"PUT", "/api/v1/admin/channel-monitors/sort-order/run", false},
		{"GET", "/api/v1/admin/channel-monitors/0", false},
		{"GET", "/api/v1/admin/channel-monitors/01", false},
		{"GET", "/api/v1/admin/channel-monitors/-1", false},
		{"GET", "/api/v1/admin/channel-monitors/+1", false},
		{"GET", "/api/v1/admin/channel-monitors/12345678901234567890", false},
		{"GET", "/api/v1/admin/channel-monitors/1/", false},
		{"GET", "/api/v1/admin/channel-monitors/1e2", false},
		{"GET", "/api/v1/admin/channel-monitors/1/../backups", false},
		{"POST", "/api/v1/admin/backups", false},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := New(Config{Role: Serving})
			reached := 0
			h := r.Wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				reached++
				if r.Status().ActiveHTTP != 1 {
					t.Error("allowed admin request was not counted for drain")
				}
				if req.Header.Get("Authorization") != "Bearer admin-fixture" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			for _, authorized := range []bool{false, true} {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				want := http.StatusServiceUnavailable
				if tc.allowed {
					want = http.StatusUnauthorized
					if authorized {
						want = http.StatusNoContent
					}
				}
				if authorized {
					req.Header.Set("Authorization", "Bearer admin-fixture")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("authorized=%v: status=%d want=%d", authorized, w.Code, want)
				}
			}
			if (reached == 2) != tc.allowed || (!tc.allowed && reached != 0) || r.Status().ActiveHTTP != 0 {
				t.Fatalf("unexpected role boundary or active request count: reached=%d status=%+v", reached, r.Status())
			}
		})
	}
}

func TestStaleDrainCannotRetireReplacementInstance(t *testing.T) {
	r := New(Config{Role: Serving, Token: strings.Repeat("c", 40)})
	r.Ready()
	for _, body := range []string{"", `{}`, `{"instance_id":"retired-generation"}`} {
		req := httptest.NewRequest("POST", "/internal/sources/drain", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+r.config.Token)
		w := httptest.NewRecorder()
		r.Wrap(http.NotFoundHandler()).ServeHTTP(w, req)
		if w.Code != http.StatusConflict || r.Status().Draining {
			t.Fatal("a stale or unspecified process generation drained its replacement")
		}
	}
}

func TestConcurrentImageAcknowledgementsDrainExactlyOnce(t *testing.T) {
	r := New(Config{Role: Serving})
	var wg sync.WaitGroup
	for range 100 {
		finish := r.BeginImage()
		wg.Go(func() { finish(true); finish(true) })
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
