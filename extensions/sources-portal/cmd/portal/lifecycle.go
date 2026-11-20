package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// runPortal does not release the store's lifetime lock until accepted HTTP
// handlers AND the single background worker have finished. Shutdown closes the
// listener before Serve returns, so waiting only for the worker loses this guard.
// An operator must retain a slow process, not escalate it to a forced exit.
func runPortal(ctx context.Context, server *http.Server, listener net.Listener, worker func(context.Context), logger *log.Logger, intelligenceBusy func() bool) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var requests atomic.Int64
	handler := server.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		defer requests.Add(-1)
		handler.ServeHTTP(w, r)
	})
	workerDone, shutdownDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(workerDone); worker(runCtx) }()
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		started := time.Now()
		httpDone := make(chan struct{})
		pendingHTTP, pendingWorker := httpDone, workerDone
		logState := func(event string) {
			if logger == nil {
				return
			}
			busy := intelligenceBusy != nil && intelligenceBusy()
			logger.Printf("Portal shutdown %s: active_requests=%d http_pending=%t background_pending=%t intelligence_round_active=%t elapsed=%s", event, requests.Load(), pendingHTTP != nil, pendingWorker != nil, busy, time.Since(started).Round(time.Millisecond))
		}
		go func() {
			// A deadline here could return while accepted HTTP handlers still
			// write state. Always drain them before releasing the state lock.
			_ = server.Shutdown(context.Background())
			close(httpDone)
		}()
		logState("started")
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for pendingHTTP != nil || pendingWorker != nil {
			select {
			case <-pendingHTTP:
				pendingHTTP = nil
				logState("http_drained")
			case <-pendingWorker:
				pendingWorker = nil
				logState("background_stopped")
			case <-ticker.C:
				logState("waiting")
			}
		}
		logState("complete")
	}()
	err := server.Serve(listener)
	cancel()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
