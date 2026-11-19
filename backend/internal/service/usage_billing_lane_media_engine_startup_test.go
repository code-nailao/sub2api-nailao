//go:build unit

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/stretchr/testify/require"
)

// Unexpected ordinary or batched billing calls panic through the nil embedded
// interface. Startup must not charge, release, or dispatch an image request.
type laneMediaEngineStartupRepo struct {
	UsageBillingBatchRepository
	ctx          context.Context
	settledHold  *BatchImageBalanceHoldCommand
	settledBill  *UsageBillingCommand
	releasedHold *BatchImageBalanceHoldCommand
	result       *UsageBillingApplyResult
	settleCalls  int
	releaseCalls int
}

func (r *laneMediaEngineStartupRepo) SettleSourcesImage(ctx context.Context, hold *BatchImageBalanceHoldCommand, bill *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.ctx, r.settledHold, r.settledBill = ctx, hold, bill
	r.settleCalls++
	return r.result, nil
}

func (r *laneMediaEngineStartupRepo) ReleaseSourcesImage(ctx context.Context, hold *BatchImageBalanceHoldCommand) error {
	r.ctx, r.releasedHold = ctx, hold
	r.releaseCalls++
	return nil
}

type laneMediaEngineStartupAccounts struct{ AccountRepository }
type laneMediaEngineStartupUsageLogs struct{ UsageLogRepository }

func TestUsageBillingLaneSourcesMediaEngineConfiguredStartup(t *testing.T) {
	// Service tests use the default standalone runtime, which exercises the
	// same owner-store and engine initialization branch as a background process.
	require.True(t, processrole.BackgroundEnabled())
	dir := t.TempDir()
	registry := filepath.Join(dir, "image-engines.json")
	key := filepath.Join(dir, "media-engine.key")
	require.NoError(t, os.WriteFile(registry, []byte(`[{"group_id":7,"engine":"media","models":["gpt-image-2"],"account_ids":[9]}]`), 0600))
	require.NoError(t, os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0600))
	t.Setenv("SOURCES_IMAGE_ENGINES_FILE", registry)
	t.Setenv("SOURCES_MEDIA_ENGINE_KEY_FILE", key)
	t.Setenv("SOURCES_MEDIA_ENGINE_DATA_DIR", filepath.Join(dir, "tasks"))
	for _, name := range []string{
		"SOURCES_MEDIA_ENGINE_ADMISSION_MODE", "SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE",
		"SOURCES_MEDIA_ENGINE_LEGACY_ORIGIN", "SOURCES_MEDIA_ENGINE_RECOVERY_POLICY_FILE",
		"SOURCES_MEDIA_ENGINE_RECOVERY_ENABLE_FILE",
	} {
		t.Setenv(name, "")
	}

	repo := &laneMediaEngineStartupRepo{result: &UsageBillingApplyResult{Applied: true}}
	lanes := NewUsageBillingLaneCombiner(repo)
	t.Cleanup(lanes.Stop)
	// Use the exact provider path from wire_gen rather than injecting the
	// underlying optional-capability repository straight into the gateway.
	wired := ProvideUsageBillingRepository(lanes)
	gateway := &OpenAIGatewayService{
		usageBillingRepo:    wired,
		usageLogRepo:        &laneMediaEngineStartupUsageLogs{},
		accountRepo:         &laneMediaEngineStartupAccounts{},
		billingService:      &BillingService{},
		billingCacheService: &BillingCacheService{},
		concurrencyService:  &ConcurrencyService{},
		cfg:                 &config.Config{RunMode: config.RunModeStandard},
	}
	svc, err := NewSourcesMediaEngine(gateway, nil, &ImageTaskService{}, &APIKeyService{})
	require.NoError(t, err)
	require.NotNil(t, svc)
	t.Cleanup(svc.Stop)
	require.NotNil(t, svc.Engine)
	require.NotNil(t, svc.taskProducer())
	require.Equal(t, "media", svc.routes[7].Engine)
	require.FileExists(t, filepath.Join(dir, "tasks", "tasks.sqlite"))
	require.Zero(t, repo.settleCalls)
	require.Zero(t, repo.releaseCalls)

	ledger, ok := svc.gateway.usageBillingRepo.(SourcesImageLedger)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := &BatchImageBalanceHoldCommand{BatchID: "startup-regression"}
	bill := &UsageBillingCommand{RequestID: "startup-regression"}
	result, err := ledger.SettleSourcesImage(ctx, hold, bill)
	require.NoError(t, err)
	require.Same(t, repo.result, result)
	require.Same(t, ctx, repo.ctx)
	require.Same(t, hold, repo.settledHold)
	require.Same(t, bill, repo.settledBill)
	require.NoError(t, ledger.ReleaseSourcesImage(ctx, hold))
	require.Same(t, ctx, repo.ctx)
	require.Same(t, hold, repo.releasedHold)
	require.Equal(t, 1, repo.settleCalls)
	require.Equal(t, 1, repo.releaseCalls)
	require.Zero(t, lanes.Stats().SingleApplies)
	require.Zero(t, lanes.Stats().Batches)
}
