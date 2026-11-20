package portal

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

type shutdownBalanceBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *shutdownBalanceBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*shutdownBalanceBody) Close() error { return nil }

func TestShutdownCancelledBalancePreservesRecord(t *testing.T) {
	for _, mode := range []string{"synchronous", "asynchronous", "background", "response-body", "transport-cause"} {
		t.Run(mode, func(t *testing.T) {
			svc := newTestService(t)
			svc.Webhook = "https://alerts.example/hook"
			source, err := svc.Save("", input(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Refresh(context.Background(), source.ID); err != nil {
				t.Fatal(err)
			}
			if mode == "background" {
				if err = svc.Store.change(func(rows map[string]record) error {
					row := rows[source.ID]
					row.Source.NextCheckAt = row.Source.LastCheckedAt.Add(-svc.Interval)
					rows[source.ID] = row
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := svc.Store.snapshot()[source.ID]
			if before.Pending == nil || before.Alarm == "" || before.Source.LastBalance == nil {
				t.Fatal("fixture must include a balance and pending notification")
			}
			visibleBefore := svc.List()
			diskBefore, err := os.ReadFile(svc.Store.path)
			if err != nil {
				t.Fatal(err)
			}
			resetLimit(svc, source.ID)
			started := make(chan struct{})
			svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				if mode == "response-body" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &shutdownBalanceBody{ctx: r.Context(), started: started}}, nil
				}
				close(started)
				<-r.Context().Done()
				if mode == "transport-cause" {
					return nil, context.Cause(r.Context())
				}
				return nil, r.Context().Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			switch mode {
			case "synchronous", "response-body", "transport-cause":
				go func() { _, err := svc.Refresh(context.Background(), source.ID); done <- err }()
			case "asynchronous":
				if _, err = svc.RefreshAsync(context.Background(), source.ID, "shutdown-state-fixture"); err != nil {
					cancel()
					t.Fatal(err)
				}
				go func() { svc.asyncWorkers.Wait(); done <- nil }()
			case "background":
				go func() { svc.Run(ctx); done <- nil }()
			}
			defer func() { cancel(); svc.StopAsync() }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("balance read did not start")
			}
			if mode == "background" {
				cancel()
			} else {
				svc.StopAsync()
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("balance read did not stop")
			}
			if !reflect.DeepEqual(before, svc.Store.snapshot()[source.ID]) {
				t.Error("shutdown cancellation changed the source record or notification state")
			}
			if !reflect.DeepEqual(visibleBefore, svc.List()) {
				t.Error("shutdown cancellation changed the visible balance state")
			}
			diskAfter, err := os.ReadFile(svc.Store.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(diskBefore, diskAfter) {
				t.Error("shutdown cancellation rewrote persisted state")
			}
		})
	}
}

func TestBalanceRefreshStillRecordsRequestAndUpstreamFailures(t *testing.T) {
	for _, name := range []string{"request-timeout", "request-cancellation", "upstream-timeout", "upstream-status", "invalid-response"} {
		t.Run(name, func(t *testing.T) {
			svc := newTestService(t)
			defer svc.StopAsync()
			source, err := svc.Save("", input(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Refresh(context.Background(), source.ID); err != nil {
				t.Fatal(err)
			}
			before := svc.Store.snapshot()[source.ID]
			resetLimit(svc, source.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantError := ErrTransport
			var transport roundTrip
			switch name {
			case "request-timeout":
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer deadlineCancel()
				wantError = ErrQueryTimeout
				transport = func(r *http.Request) (*http.Response, error) {
					<-r.Context().Done()
					svc.asyncCancel()
					return nil, r.Context().Err()
				}
			case "request-cancellation":
				transport = func(r *http.Request) (*http.Response, error) {
					cancel()
					<-r.Context().Done()
					svc.asyncCancel()
					return nil, r.Context().Err()
				}
			case "upstream-timeout":
				wantError = ErrQueryTimeout
				transport = func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }
			case "upstream-status":
				transport = func(*http.Request) (*http.Response, error) {
					svc.asyncCancel()
					return reply(http.StatusServiceUnavailable, ""), nil
				}
			case "invalid-response":
				wantError = ErrResponse
				transport = func(*http.Request) (*http.Response, error) {
					svc.asyncCancel()
					return reply(http.StatusOK, "invalid fixture response"), nil
				}
			}
			svc.Provider.Client.Transport = transport
			if _, err = svc.Refresh(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			after := svc.Store.snapshot()[source.ID]
			if after.Source.Status != "error" || after.Source.Error != wantError.Error() || after.Source.LastCheckedAt.Equal(before.Source.LastCheckedAt) {
				t.Fatal("request or upstream failure was suppressed or reclassified")
			}
			if !reflect.DeepEqual(before.Source.LastBalance, after.Source.LastBalance) {
				t.Fatal("failure lost the prior balance snapshot")
			}
		})
	}
}
