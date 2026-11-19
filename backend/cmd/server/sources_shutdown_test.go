package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *stepLog) record(step string) func() { return func() { l.add(step) } }

func (l *stepLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

func recordedShutdown(role processrole.Role, log *stepLog) sourcesShutdown {
	return sourcesShutdown{
		role:           role,
		drain:          log.record("drain"),
		waitWrites:     log.record("wait admin writes"),
		stopBackground: log.record("stop background services"),
		closeUpgrades:  log.record("close admin upgrades"),
		shutdownHTTP:   log.record("close listener"),
		waitIdle:       log.record("wait idle"),
		exited:         log.record("log exited"),
		stopSettings:   log.record("stop settings refresh"),
		cleanup:        log.record("cleanup"),
	}
}

func TestShutdownOrderPerRole(t *testing.T) {
	// Serving and standalone: exactly the sequence before the split (Drain,
	// Shutdown, Wait, "Server exited", then the deferred settings refresh stop
	// and Cleanup in LIFO order).
	unchanged := []string{"drain", "close listener", "wait idle", "log exited", "stop settings refresh", "cleanup"}
	for _, role := range []processrole.Role{processrole.Serving, processrole.Standalone} {
		log := &stepLog{}
		recordedShutdown(role, log).run()
		require.Equal(t, unchanged, log.list(), "role %s", role)
	}

	log := &stepLog{}
	recordedShutdown(processrole.Background, log).run()
	require.Equal(t, []string{
		"drain", "wait admin writes", "stop background services", "close admin upgrades",
		"close listener", "wait idle", "log exited", "stop settings refresh", "cleanup",
	}, log.list())
}

func TestApplicationCleanupRunsEachPhaseOnceInOrder(t *testing.T) {
	newCleanup := func() (*applicationCleanup, *stepLog) {
		log := &stepLog{}
		return newApplicationCleanup(func(phase cleanupPhase) {
			switch phase {
			case cleanupBackgroundServices:
				log.add("background services")
			case cleanupRemaining:
				log.add("remaining")
			}
		}), log
	}
	want := []string{"background services", "remaining"}

	// Serving and standalone call Run only: same order as the old closure.
	cleanup, log := newCleanup()
	cleanup.Run()
	cleanup.Run()
	require.Equal(t, want, log.list())

	// The background owner stops its services first, then the deferred Run
	// must not stop them again.
	cleanup, log = newCleanup()
	cleanup.StopBackground()
	cleanup.StopBackground()
	require.Equal(t, want[:1], log.list())
	cleanup.Run()
	require.Equal(t, want, log.list())
}

// A real listener and the real runtime wrapper: while the owner's background
// services are stopping it still answers admin reads and refuses writes; the
// upgraded admin connection is closed before the listener; an ordinary
// request in flight is not cancelled; the listener closes last.
func TestBackgroundOwnerAnswersAdminReadsUntilItsServicesStop(t *testing.T) {
	runtime := processrole.New(processrole.Config{Role: processrole.Background})
	runtime.Ready()

	slowEntered, releaseSlow := make(chan struct{}), make(chan struct{})
	slowErr := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/admin/items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/api/v1/admin/slow", func(w http.ResponseWriter, req *http.Request) {
		close(slowEntered)
		<-releaseSlow
		slowErr <- req.Context().Err()
		_, _ = io.WriteString(w, "slow")
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	// Same shape as the ops realtime handler: push until the request context
	// ends, then send a normal close frame.
	mux.HandleFunc("/api/v1/admin/ops/ws/qps", func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-req.Context().Done():
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			case <-ticker.C:
				if conn.WriteMessage(websocket.TextMessage, []byte("tick")) != nil {
					return
				}
			}
		}
	})
	server := &http.Server{Handler: runtime.Wrap(mux), ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	base := "http://" + ln.Addr().String()
	// A fresh connection per request proves the listener itself is accepting.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/api/v1/admin/ops/ws/qps", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	_, _, err = ws.ReadMessage()
	require.NoError(t, err)

	slowDone := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(base + "/api/v1/admin/slow")
		if err != nil {
			slowDone <- nil
			return
		}
		slowDone <- resp
	}()
	<-slowEntered

	log := &stepLog{}
	stopping, releaseStop := make(chan struct{}), make(chan struct{})
	ctx := context.Background()
	shutdown := sourcesShutdown{
		role:  processrole.Background,
		drain: runtime.Drain,
		waitWrites: func() {
			require.NoError(t, runtime.WaitAdminWrites(ctx))
			log.add("admin writes done")
		},
		stopBackground: func() {
			log.add("stop background services")
			close(stopping)
			<-releaseStop
		},
		closeUpgrades: func() {
			log.add("close admin upgrades")
			runtime.CloseAdminUpgrades()
		},
		shutdownHTTP: func() {
			log.add("close listener")
			_ = server.Shutdown(ctx)
		},
		waitIdle: func() {
			require.NoError(t, runtime.Wait(ctx))
			log.add("idle")
		},
		exited:       func() {},
		stopSettings: func() {},
		cleanup:      log.record("cleanup"),
	}
	done := make(chan struct{})
	go func() {
		shutdown.run()
		close(done)
	}()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("background services did not start stopping while the listener was open")
	}

	// Background services are stopping: reads and health answer, writes are
	// refused with the handoff envelope, the realtime page keeps streaming.
	resp, err := client.Get(base + "/api/v1/admin/items")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	resp, err = client.Get(base + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	resp, err = client.Post(base+"/api/v1/admin/items", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, processrole.HandoffRetryAfterSeconds, resp.Header.Get("Retry-After"))
	require.Contains(t, string(body), "BACKGROUND_HANDOFF_READ_ONLY")
	_, _, err = ws.ReadMessage()
	require.NoError(t, err, "the realtime page streams while services stop")

	close(releaseStop)
	// The upgraded connection ends with a normal close frame.
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err = ws.ReadMessage(); err != nil {
			break
		}
	}
	var closeErr *websocket.CloseError
	require.True(t, errors.As(err, &closeErr), "want a close frame, got %v", err)
	require.Equal(t, websocket.CloseNormalClosure, closeErr.Code)

	// The listener closes while the slow ordinary read is still in flight.
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	}, 2*time.Second, 10*time.Millisecond, "listener still accepting after the services stopped")
	select {
	case <-done:
		t.Fatal("shutdown finished before the in-flight read")
	default:
	}
	close(releaseSlow)
	slow := <-slowDone
	require.NotNil(t, slow, "in-flight read failed")
	require.Equal(t, http.StatusOK, slow.StatusCode)
	_ = slow.Body.Close()
	require.NoError(t, <-slowErr, "the ordinary request context must not be cancelled")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	require.Equal(t, []string{
		"admin writes done", "stop background services", "close admin upgrades", "close listener", "idle", "cleanup",
	}, log.list())
}
