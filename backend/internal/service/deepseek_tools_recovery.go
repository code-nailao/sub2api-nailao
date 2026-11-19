package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
)

// Runs only on the existing single background owner. Pending live requests are
// protected by their bounded deadline. No model/provider credentials are needed.
type DeepSeekUsageRecovery struct {
	gateway *OpenAIGatewayService
	repo    DeepSeekUsageJournal
	updater *APIKeyService
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

func ProvideDeepSeekUsageRecovery(gateway *OpenAIGatewayService, billing UsageBillingRepository, updater *APIKeyService) *DeepSeekUsageRecovery {
	repo, _ := billing.(DeepSeekUsageJournal)
	w := &DeepSeekUsageRecovery{gateway: gateway, repo: repo, updater: updater}
	processrole.StartBackground("DeepSeekUsageRecovery", w.Start)
	return w
}

func (w *DeepSeekUsageRecovery) Start() {
	if w == nil || !processrole.BackgroundEnabled() || w.repo == nil || w.gateway == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil || w.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			w.runOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *DeepSeekUsageRecovery) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (w *DeepSeekUsageRecovery) runOnce(ctx context.Context) {
	scanCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := w.repo.ListPendingDeepSeekUsage(scanCtx, 32)
	if err != nil {
		return
	}
	for _, id := range rows {
		if scanCtx.Err() != nil {
			return
		}
		settlement, err := w.repo.SettleDeepSeekUsage(scanCtx, id.RequestID, id.APIKeyID)
		if err != nil {
			logger.LegacyPrintf("service.deepseek_recovery", "usage remains pending: request_id=%s", id.RequestID)
			continue
		}
		if settlement == nil || settlement.Command == nil {
			continue
		}
		cmd, usage := settlement.Command, settlement.Usage
		invalidateDeepSeekAuthCache(scanCtx, &APIKey{ID: cmd.APIKeyID, UserID: cmd.UserID}, settlement.Result, w.updater)
		if settlement.Result != nil && settlement.Result.Applied && usage != nil {
			key := &APIKey{ID: cmd.APIKeyID, UserID: cmd.UserID, GroupID: usage.GroupID}
			p := &postUsageBillingParams{
				User: &User{ID: cmd.UserID}, APIKey: key,
				Account: &Account{ID: cmd.AccountID, Platform: PlatformDeepseek, Type: cmd.AccountType},
				Cost:    &CostBreakdown{TotalCost: usage.TotalCost, ActualCost: usage.ActualCost}, Platform: PlatformDeepseek,
			}
			finalizePostUsageBilling(scanCtx, p, w.gateway.billingDeps(), settlement.Result)
		}
		w.gateway.invalidateDeepSeekBillingCaches(scanCtx, cmd)
	}
}
