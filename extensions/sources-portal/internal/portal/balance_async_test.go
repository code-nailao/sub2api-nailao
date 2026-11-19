package portal

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestBalanceAsyncPollsOutliveBrowserAndStop(t *testing.T) {
	svc := newTestService(t)
	t.Cleanup(svc.StopAsync)
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	original := svc.Provider.Client.Transport
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-finish:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return original.RoundTrip(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	pending, err := svc.RefreshAsync(ctx, src.ID, "portal-request-0001")
	if err != nil || !pending.Querying {
		t.Fatalf("%+v %v", pending, err)
	}
	<-started
	cancel()
	for i := 0; i < 3; i++ {
		pending, err = svc.RefreshAsync(context.Background(), src.ID, "portal-request-0001")
		if err != nil || !pending.Querying {
			t.Fatalf("poll: %+v %v", pending, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("poll duplicated upstream request")
	}
	close(finish)
	deadline := time.Now().Add(time.Second)
	for {
		result, err := svc.RefreshAsync(context.Background(), src.ID, "portal-request-0001")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Querying {
			if result.LastBalance == nil || result.LastBalance.Remaining != "2.5" {
				t.Fatalf("%+v", result)
			}
			if result.NextCheckAt.Sub(result.LastCheckedAt) != 5*time.Minute {
				t.Fatal("refresh must be scheduled after completion")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("query did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	svc.StopAsync()
	if _, err = svc.RefreshAsync(context.Background(), src.ID, "portal-request-0002"); err != ErrTransport {
		t.Fatal(err)
	}
}

func TestBalanceClientSixMinuteBudgetLeavesAuthenticationUnchanged(t *testing.T) {
	client := BalanceClient()
	if client.Timeout != 360*time.Second || client.Transport.(*http.Transport).ResponseHeaderTimeout != 360*time.Second {
		t.Fatal("wallet client budget")
	}
	if PublicClient().Timeout != 15*time.Second {
		t.Fatal("authentication client was changed")
	}
}
