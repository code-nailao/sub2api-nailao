package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sources.local/portal/internal/portal"
)

func TestShutdownKeepsStoreLockUntilAcceptedWriteAndWorkerComplete(t *testing.T) {
	dir, key := t.TempDir(), make([]byte, 32)
	store, err := portal.OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted, release, workerStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(accepted)
		<-release
		_, _ = io.WriteString(w, "write persisted")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		err := runPortal(ctx, server, listener, func(ctx context.Context) { <-ctx.Done(); close(workerStopped) })
		_ = store.Close()
		finished <- err
	}()
	response := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		res, err := client.Post("http://"+listener.Addr().String(), "application/json", strings.NewReader("{}"))
		if err != nil {
			response <- err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		response <- string(body)
	}()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("request not accepted")
	}
	cancel()
	select {
	case <-workerStopped:
	case <-time.After(time.Second):
		t.Fatal("worker not cancelled")
	}
	select {
	case err := <-finished:
		t.Fatalf("exited while write remained active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if other, err := portal.OpenStore(dir, key); err == nil {
		other.Close()
		t.Fatal("store lock released before handler completed")
	}
	close(release)
	select {
	case got := <-response:
		if got != "write persisted" {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write truncated")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	other, err := portal.OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
}

func TestShutdownKeepsStoreLockUntilBackgroundWorkerFinishes(t *testing.T) {
	dir, key := t.TempDir(), make([]byte, 32)
	store, err := portal.OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerCancelled, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		err := runPortal(ctx, &http.Server{Handler: http.NotFoundHandler()}, listener, func(ctx context.Context) {
			<-ctx.Done()
			close(workerCancelled)
			<-release
		})
		_ = store.Close()
		finished <- err
	}()
	cancel()
	<-workerCancelled
	select {
	case err := <-finished:
		t.Fatalf("exited before background worker finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if other, err := portal.OpenStore(dir, key); err == nil {
		other.Close()
		t.Fatal("store lock released while background worker still active")
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
