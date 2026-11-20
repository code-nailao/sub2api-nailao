package portal

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Observe cancellation so a shutdown that interrupts a paid call cannot
// accidentally pass the regression test.
type shutdownIntelligenceProvider struct {
	intelligenceProvider
	entered chan context.Context
	release chan struct{}
}

func (p *shutdownIntelligenceProvider) wait(ctx context.Context) error {
	p.entered <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

func (p *shutdownIntelligenceProvider) Submit(ctx context.Context, in intelligenceSubmission, operation string) (intelligenceRemoteResult, error) {
	if err := p.wait(ctx); err != nil {
		return intelligenceRemoteResult{}, err
	}
	return p.intelligenceProvider.Submit(ctx, in, operation)
}

func (p *shutdownIntelligenceProvider) Poll(ctx context.Context, id string) (intelligenceRemoteResult, error) {
	if err := p.wait(ctx); err != nil {
		return intelligenceRemoteResult{}, err
	}
	return p.intelligenceProvider.Poll(ctx, id)
}

func TestShutdownCancelsBalanceBeforeIntelligenceRoundFinishes(t *testing.T) {
	for _, phase := range []string{"submit", "poll"} {
		t.Run(phase, func(t *testing.T) {
			intelligence, provider, owner, _ := intelligenceFixture(t)
			plan := seedIntelligencePlan(t, intelligence, false)
			run := enqueueIntelligence(t, intelligence, plan, "candy")
			if phase == "poll" {
				intelligence.tick()
				if err := intelligence.Store.change(func(st *intelligenceState) error {
					row := st.Runs[run.ID]
					row.NextPoll = intelligence.now()
					st.Runs[run.ID] = row
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				provider.result.Status, provider.result.Benchmark = "succeeded", "candy"
				provider.result.Candy.Status, provider.result.Candy.Answer = "passed", "fixture result"
			}
			blocked := &shutdownIntelligenceProvider{intelligenceProvider: provider, entered: make(chan context.Context, 1), release: make(chan struct{})}
			releaseDetection := sync.OnceFunc(func() { close(blocked.release) })
			intelligence.provider = blocked
			balances := NewService(owner, fixtureProvider(t, "USD", "1250000"))
			balances.Intelligence = intelligence
			source, err := balances.Save("", input(), 0)
			if err != nil {
				t.Fatal(err)
			}
			source, err = balances.Refresh(context.Background(), source.ID)
			if err != nil || source.LastBalance == nil {
				t.Fatal("could not seed a prior balance snapshot", err)
			}
			resetLimit(balances, source.ID)
			balanceStarted := make(chan struct{})
			balances.Provider.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				close(balanceStarted)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { balances.Run(ctx); close(done) }()
			defer func() {
				cancel()
				releaseDetection()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("worker did not finish after the fixture was released")
				}
			}()
			var paidContext context.Context
			select {
			case paidContext = <-blocked.entered:
			case <-time.After(time.Second):
				t.Fatal("detection call did not start")
			}
			if !intelligence.RoundActive() {
				t.Fatal("active detection round was invisible to shutdown logs")
			}
			refreshDone := make(chan error, 1)
			refreshCtx, stopRefresh := context.WithCancel(context.Background())
			defer stopRefresh()
			go func() { _, err := balances.Refresh(refreshCtx, source.ID); refreshDone <- err }()
			select {
			case <-balanceStarted:
			case <-time.After(time.Second):
				t.Fatal("balance call did not start")
			}
			cancel()
			select {
			case err := <-refreshDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("balance refresh waited for the paid detection call")
			}
			select {
			case <-done:
				t.Fatal("worker exited before the paid detection call finished")
			default:
			}
			if paidContext.Err() != nil {
				t.Fatal("shutdown cancelled the paid detection call")
			}
			current := owner.snapshot()[source.ID].Source
			if current.LastBalance == nil || *current.LastBalance != *source.LastBalance {
				t.Fatal("cancelled read lost the prior balance snapshot")
			}
			// Complete the paid call before inspecting its durable outcome.
			releaseDetection()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("worker did not finish after its detection round")
			}
			if intelligence.RoundActive() {
				t.Fatal("finished detection round still reported active")
			}
			reopened, err := OpenIntelligenceStore(owner)
			if err != nil {
				t.Fatal(err)
			}
			row := reopened.snapshot().Runs[run.ID]
			if row.RemoteID == "" || phase == "poll" && !row.Run.HasResult {
				t.Fatal("shutdown lost the accepted task or completed result")
			}
			if provider.submits != 1 {
				t.Fatal("shutdown replayed a paid submission")
			}
		})
	}
}
