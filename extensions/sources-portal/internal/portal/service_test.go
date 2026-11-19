package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetLimit(s *Service, id string) { s.mu.Lock(); delete(s.last, id); s.mu.Unlock() }
func TestWalletFiveMinuteScheduleRequiresSuccessAndSurvivesFailure(t *testing.T) {
	svc := newTestService(t)
	if svc.Interval != 5*time.Minute {
		t.Fatal("balance interval must be five minutes")
	}
	now := time.Now()
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if svc.balanceDue(src, now.Add(time.Hour)) {
		t.Fatal("unverified source must not be polled")
	}
	src, err = svc.Refresh(context.Background(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if svc.balanceDue(src, now.Add(time.Minute)) || !svc.balanceDue(src, now.Add(6*time.Minute)) {
		t.Fatal("incorrect due time")
	}
	// Honor the new cadence even for a stored result carrying an old due time.
	src.NextCheckAt = now.Add(30 * time.Minute)
	if !svc.balanceDue(src, now.Add(6*time.Minute)) {
		t.Fatal("old schedule was not shortened")
	}
	resetLimit(svc, src.ID)
	svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < 359*time.Second || time.Until(deadline) > 360*time.Second {
			t.Fatal("missing six-minute budget")
		}
		return nil, context.DeadlineExceeded
	})
	failed, err := svc.Refresh(context.Background(), src.ID)
	if err != nil || failed.Error != "upstream_timeout" || failed.LastBalance == nil {
		t.Fatalf("%+v %v", failed, err)
	}
	if !svc.balanceDue(failed, now.Add(6*time.Minute)) {
		t.Fatal("transient failure must retain automatic refresh")
	}
	failed.Status = "pending"
	if svc.balanceDue(failed, now.Add(time.Hour)) {
		t.Fatal("edited credentials must be reverified")
	}
}

func TestRefreshLowStaleAndBoundIdentity(t *testing.T) {
	svc := newTestService(t)
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Refresh(context.Background(), src.ID)
	if err != nil || result.Status != "low" || result.LastBalance.Remaining != "2.5" || result.AccountID != 42 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err = svc.Refresh(context.Background(), src.ID); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	resetLimit(svc, src.ID)
	svc.Provider.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return reply(401, "DO_NOT_EXPOSE_SECRET"), nil })
	stale, err := svc.Refresh(context.Background(), src.ID)
	if err != nil || stale.Status != "error" || stale.LastBalance.Remaining != "2.5" || stale.Error != ErrCredential.Error() {
		t.Fatalf("%+v %v", stale, err)
	}
	in := input()
	in.Revision = src.Revision
	in.PersonalToken = "new-personal-fixture"
	in.AccountID = 43
	if _, err = svc.Save(src.ID, in, 9); err == nil {
		t.Fatal("changed bound account accepted")
	}
}
func TestAlertUnitMismatchAndNoDuplicateWallet(t *testing.T) {
	svc := newTestService(t)
	in := input()
	in.ThresholdUnit = "CNY"
	a, _ := svc.Save("", in, 9)
	result, err := svc.Refresh(context.Background(), a.ID)
	if err != nil || result.Status != "unit_mismatch" {
		t.Fatalf("%+v %v", result, err)
	}
	b, _ := svc.Save("", input(), 9)
	result, err = svc.Refresh(context.Background(), b.ID)
	if err != nil || result.Status != "error" || result.Error != "account_already_monitored" || result.LastBalance != nil {
		t.Fatalf("%+v %v", result, err)
	}
	if !result.NextCheckAt.After(time.Now()) {
		t.Fatal("duplicate account has no backoff")
	}
}
func TestRefreshDoesNotOverwriteEditedCredential(t *testing.T) {
	svc := newTestService(t)
	src, _ := svc.Save("", input(), 9)
	started := make(chan struct{})
	release := make(chan struct{})
	original := svc.Provider.Client.Transport
	svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/user/self" {
			close(started)
			<-release
		}
		return original.RoundTrip(r)
	})
	done := make(chan error, 1)
	go func() { _, err := svc.Refresh(context.Background(), src.ID); done <- err }()
	<-started
	in := input()
	in.Revision = src.Revision
	in.Name = "edited"
	in.PersonalToken = ""
	_, err := svc.Save(src.ID, in, 9)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-done; !errors.Is(err, ErrConflict) {
		t.Fatalf("stale probe overwrote edit: %v", err)
	}
	if svc.List()[0].LastBalance != nil {
		t.Fatal("stale result saved")
	}
}
func TestNotificationOutboxDeduplicatesAndRetriesSameID(t *testing.T) {
	svc := newTestService(t)
	svc.Webhook = "https://alerts.example/hook"
	src, _ := svc.Save("", input(), 9)
	_, err := svc.Refresh(context.Background(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := svc.Store.snapshot()[src.ID].Pending
	if first == nil {
		t.Fatal("missing low notification")
	}
	resetLimit(svc, src.ID)
	_, _ = svc.Refresh(context.Background(), src.ID)
	if svc.Store.snapshot()[src.ID].Pending.ID != first.ID {
		t.Fatal("duplicate low notification")
	}
	calls := 0
	fail := true
	svc.NotifyClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Idempotency-Key") != first.ID {
			t.Fatal("retry changed ID")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "personal-fixture") {
			t.Fatal("secret in alert")
		}
		if fail {
			return reply(500, ""), nil
		}
		return reply(204, ""), nil
	})}
	if svc.Deliver(context.Background(), src.ID) == nil {
		t.Fatal("failed callback cleared outbox")
	}
	fail = false
	if err = svc.Deliver(context.Background(), src.ID); err != nil {
		t.Fatal(err)
	}
	_ = svc.Deliver(context.Background(), src.ID)
	if calls != 2 || svc.Store.snapshot()[src.ID].Pending != nil {
		t.Fatal("duplicate send after acknowledgment")
	}
	svc.Provider = fixtureProvider(t, "USD", "2500000")
	resetLimit(svc, src.ID)
	_, _ = svc.Refresh(context.Background(), src.ID)
	if n := svc.Store.snapshot()[src.ID].Pending; n == nil || n.State != "ok" || n.ID == first.ID {
		t.Fatal("missing recovery event")
	}
}
func TestSaveRejectsFractionExpressionAndModelKey(t *testing.T) {
	svc := newTestService(t)
	for _, threshold := range []string{"1/2", "NaN", "1e999999999", "-1"} {
		in := input()
		in.Threshold = threshold
		if _, err := svc.Save("", in, 9); err == nil {
			t.Fatal(threshold)
		}
	}
	in := input()
	in.PersonalToken = "sk-not-a-personal-access-token"
	if _, err := svc.Save("", in, 9); !errors.Is(err, ErrCredential) {
		t.Fatal(err)
	}
}

func TestRefreshConcurrencyBounded(t *testing.T) {
	for _, sameSite := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-site-%v", sameSite), func(t *testing.T) {
			svc := newTestService(t)
			limit := 20
			if sameSite {
				limit = 4
			}
			started := make(chan struct{}, limit)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			original := svc.Provider.Client.Transport
			svc.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/user/self" {
					started <- struct{}{}
					<-release
				}
				return original.RoundTrip(r)
			})
			ids := []string{}
			for i := 0; i <= limit; i++ {
				in := input()
				in.Origin = fmt.Sprintf("https://wallet-%d.example", i)
				if sameSite {
					in.Origin = "https://same-wallet.example"
				}
				src, err := svc.Save("", in, 9)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, src.ID)
			}
			done := make(chan error, limit)
			for _, id := range ids[:limit] {
				go func(id string) { _, err := svc.Refresh(context.Background(), id); done <- err }(id)
			}
			for i := 0; i < limit; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("expected concurrency unavailable")
				}
			}
			if _, err := svc.Refresh(context.Background(), ids[limit]); !errors.Is(err, ErrBusy) {
				t.Fatalf("unbounded request: %v", err)
			}
			once.Do(func() { close(release) })
			for i := 0; i < limit; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
