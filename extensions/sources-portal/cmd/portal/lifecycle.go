package main

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// runPortal does not release the store's lifetime lock until accepted HTTP
// handlers AND the single background worker have finished. Shutdown closes the
// listener before Serve returns, so waiting only for the worker loses this guard.
// An operator must retain a slow process, not escalate it to a forced exit.
func runPortal(ctx context.Context, server *http.Server, listener net.Listener, worker func(context.Context)) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone, shutdownDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(workerDone); worker(runCtx) }()
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		_ = server.Shutdown(context.Background())
	}()
	err := server.Serve(listener)
	cancel()
	<-shutdownDone
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
