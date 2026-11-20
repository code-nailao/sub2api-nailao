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

type shutdownTransport func(*http.Request) (*http.Response, error)

func (f shutdownTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestShutdownCancelsSynchronousBalanceRefresh(t *testing.T) {
	store, err := portal.OpenStore(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started, release := make(chan struct{}), make(chan struct{})
	svc := portal.NewService(store, portal.Provider{Client: &http.Client{Transport: shutdownTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return nil, portal.ErrTransport
		}
	})}})
	source, err := svc.Save("", portal.SourceInput{Name: "Fixture", Origin: "https://balance.example", PersonalToken: "fixture-only"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	api := &portal.Server{Service: svc, SubOrigin: "https://main.example", AuthClient: &http.Client{Transport: shutdownTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"id":1,"role":"admin","status":"active"}}`)), Header: make(http.Header)}, nil
	})}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- runPortal(ctx, &http.Server{Handler: api.Handler()}, listener, svc.Run, nil, nil) }()
	defer func() {
		close(release)
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Portal did not finish after the fixture was released")
		}
	}()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		req, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/balance/sources/"+source.ID+"/refresh", nil)
		if err != nil {
			response <- err
			return
		}
		req.Header.Set("Authorization", "Bearer fixture-only")
		res, err := client.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		response <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("synchronous balance refresh did not start")
	}
	cancel()
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("synchronous balance refresh kept Portal draining after shutdown")
	}
}
