package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requestSpeedSchedulerAccount(id, groupID int64, tier string) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 3,
		Priority: int(id), GroupIDs: []int64{groupID},
		RequestSpeedTiersByGroup: map[string][]string{strconv.FormatInt(groupID, 10): {tier}},
	}
}

func requestSpeedSchedulerService(accounts []Account, loadBatch bool) *OpenAIGatewayService {
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = loadBatch
	cfg.Gateway.OpenAIWS.LBTopK = 1
	return &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:       &schedulerTestGatewayCache{}, cfg: cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func releaseRequestSpeedSelection(selection *AccountSelectionResult) {
	if selection != nil && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestRequestSpeedSchedulerLegacyFiltersPriorityStickyAndFailover(t *testing.T) {
	const groupID int64 = 67001
	group := int64(groupID)
	accounts := []Account{
		requestSpeedSchedulerAccount(1, groupID, "ultrafast"),
		requestSpeedSchedulerAccount(2, groupID, "fast"),
		requestSpeedSchedulerAccount(3, groupID, "fast"),
	}
	for _, batch := range []bool{false, true} {
		t.Run(strconv.FormatBool(batch), func(t *testing.T) {
			ctx := WithRequestSpeed(context.Background(), groupID, "fast")
			svc := requestSpeedSchedulerService(accounts, batch)
			require.NoError(t, svc.setStickySessionAccountID(ctx, &group, "same-session", 1, time.Hour))
			selection, err := svc.selectAccountWithLoadAwareness(ctx, &group, PlatformOpenAI, "same-session", "gpt-6-astra", nil, false, OpenAIEndpointCapabilityResponses, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(2), selection.Account.ID, "a higher-priority or sticky Ultra supplier must not serve a Fast Key")
			releaseRequestSpeedSelection(selection)

			selection, err = svc.selectAccountWithLoadAwareness(ctx, &group, PlatformOpenAI, "", "gpt-6-astra", map[int64]struct{}{2: {}}, false, OpenAIEndpointCapabilityResponses, false)
			require.NoError(t, err)
			require.Equal(t, int64(3), selection.Account.ID, "failover must remain inside the chosen supplier tier")
			releaseRequestSpeedSelection(selection)

			selection, err = svc.selectAccountWithLoadAwareness(ctx, &group, PlatformOpenAI, "", "gpt-6-astra", map[int64]struct{}{2: {}, 3: {}}, false, OpenAIEndpointCapabilityResponses, false)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection, "all Fast suppliers unavailable must not fall back to Ultra")

			selection, err = svc.selectAccountWithLoadAwareness(context.Background(), &group, PlatformOpenAI, "", "gpt-6-astra", nil, false, OpenAIEndpointCapabilityResponses, false)
			require.NoError(t, err)
			require.Equal(t, int64(1), selection.Account.ID, "ordinary groups keep native priority selection")
			releaseRequestSpeedSelection(selection)
		})
	}
}

func TestRequestSpeedSchedulerAdvancedFiltersBeforeTopK(t *testing.T) {
	const groupID int64 = 67002
	group := int64(groupID)
	accounts := []Account{requestSpeedSchedulerAccount(1, groupID, "ultrafast"), requestSpeedSchedulerAccount(2, groupID, "fast")}
	for _, mode := range []string{"load", "sticky", "guardian", "weighted"} {
		t.Run(mode, func(t *testing.T) {
			ctx := WithRequestSpeed(context.Background(), groupID, "fast")
			svc := requestSpeedSchedulerService(accounts, true)
			scheduler := newDefaultOpenAIAccountScheduler(svc, nil)
			req := OpenAIAccountScheduleRequest{GroupID: &group, Platform: PlatformOpenAI, RequestedModel: "gpt-6-astra", RequiredTransport: OpenAIUpstreamTransportAny, RequiredCapability: OpenAIEndpointCapabilityResponses}
			if mode != "load" {
				req.SessionHash = "advanced-session"
				req.StickyAccountID = 1
				require.NoError(t, svc.setStickySessionAccountID(ctx, &group, req.SessionHash, 1, time.Hour))
			}
			if mode == "guardian" {
				req.GuardianParentAccountID = 1
			}
			if mode == "weighted" {
				req.StickyWeighted = true
				req.StickyPreviousAccountID = 1
			}
			selection, _, err := scheduler.Select(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(2), selection.Account.ID)
			releaseRequestSpeedSelection(selection)
			req.ExcludedIDs = map[int64]struct{}{2: {}}
			selection, _, err = scheduler.Select(ctx, req)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
		})
	}
}

type requestSpeedRecheckRepo struct {
	AccountRepository
	account *Account
	reads   int
	err     error
}

func (r *requestSpeedRecheckRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, r.err
}

func TestRequestSpeedSchedulerRefreshesNativeMappingBeforeUse(t *testing.T) {
	const groupID int64 = 67003
	group := int64(groupID)
	ctx := WithRequestSpeed(context.Background(), groupID, "fast")
	stale := requestSpeedSchedulerAccount(1, groupID, "fast")
	latest := requestSpeedSchedulerAccount(1, groupID, "ultrafast")
	for _, snapshots := range []bool{false, true} {
		t.Run(strconv.FormatBool(snapshots), func(t *testing.T) {
			repo := &requestSpeedRecheckRepo{account: &latest}
			svc := &OpenAIGatewayService{accountRepo: repo}
			if snapshots {
				svc.schedulerSnapshot = &SchedulerSnapshotService{}
			}
			checked := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &stale, &group, PlatformOpenAI, "gpt-6-astra", false, OpenAIEndpointCapabilityResponses)
			require.Nil(t, checked, "removed or changed mapping must veto the cached candidate")
			require.Equal(t, 1, repo.reads, "fixed supplier mapping is checked in native data, even without a scheduler snapshot")
			repo.err = errors.New("database temporarily unavailable")
			require.Nil(t, svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &stale, &group, PlatformOpenAI, "gpt-6-astra", false, OpenAIEndpointCapabilityResponses))
		})
	}
	require.Nil(t, (&OpenAIGatewayService{}).recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &stale, &group, PlatformOpenAI, "gpt-6-astra", false, OpenAIEndpointCapabilityResponses))
}

func TestRequestSpeedSchedulerHydrationCannotRestoreOldMapping(t *testing.T) {
	const groupID int64 = 67004
	ctx := WithRequestSpeed(context.Background(), groupID, "fast")
	oldSnapshot := requestSpeedSchedulerAccount(1, groupID, "ultrafast")
	current := requestSpeedSchedulerAccount(1, groupID, "fast")
	repo := &requestSpeedRecheckRepo{account: &current}
	svc := &OpenAIGatewayService{accountRepo: repo, schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{accountsByID: map[int64]*Account{1: &oldSnapshot}}}}
	hydrated, err := svc.hydrateSelectedAccount(ctx, &current)
	require.NoError(t, err)
	require.Same(t, &current, hydrated)
	require.Equal(t, 1, repo.reads)
	repo.account = &oldSnapshot
	hydrated, err = svc.hydrateSelectedAccount(ctx, &current)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, hydrated)
}

func TestRequestSpeedSchedulerResolvesAccountModelBeforeTierGuard(t *testing.T) {
	const groupID int64 = 67005
	group := int64(groupID)
	ctx := WithRequestSpeed(context.Background(), groupID, "ultrafast")
	cases := []struct {
		name        string
		requested   string
		mapped      string
		passthrough bool
		compact     bool
		want        bool
	}{
		{name: "alias to supported Sol", requested: "team-model", mapped: "gpt-6.1-sol", want: true},
		{name: "alias to unsupported Luna", requested: "team-model", mapped: "gpt-6-luna"},
		{name: "native unsupported Luna", requested: "gpt-6-luna"},
		{name: "passthrough does not apply alias mapping", requested: "team-model", mapped: "gpt-6.1-sol", passthrough: true},
		{name: "passthrough preserves supported actual model", requested: "gpt-6.1-sol", mapped: "gpt-6-luna", passthrough: true, want: true},
		{name: "compact-only alias to Sol", requested: "compact-team-model", mapped: "gpt-6.1-sol", compact: true, want: true},
		{name: "compact-only alias to unsupported Luna", requested: "compact-team-model", mapped: "gpt-6-luna", compact: true},
		{name: "passthrough compact mapping applies", requested: "compact-team-model", mapped: "gpt-6.1-sol", compact: true, passthrough: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"legacy_priority", "legacy_load", "advanced"} {
				t.Run(mode, func(t *testing.T) {
					account := requestSpeedSchedulerAccount(1, groupID, "ultrafast")
					if tc.mapped != "" && !tc.compact {
						account.Credentials = map[string]any{"model_mapping": map[string]any{tc.requested: tc.mapped}}
					}
					account.Extra = map[string]any{}
					if tc.compact {
						account.Credentials = map[string]any{"compact_model_mapping": map[string]any{tc.requested: tc.mapped}}
						account.Extra["openai_compact_supported"] = true
					}
					if tc.passthrough {
						account.Extra["openai_passthrough"] = true
					}
					svc := requestSpeedSchedulerService([]Account{account}, mode != "legacy_priority")
					var selection *AccountSelectionResult
					var err error
					if mode == "advanced" {
						selection, _, err = newDefaultOpenAIAccountScheduler(svc, nil).Select(ctx, OpenAIAccountScheduleRequest{GroupID: &group, Platform: PlatformOpenAI, RequestedModel: tc.requested, RequiredTransport: OpenAIUpstreamTransportAny, RequiredCapability: OpenAIEndpointCapabilityResponses, RequireCompact: tc.compact})
					} else {
						selection, err = svc.selectAccountWithLoadAwareness(ctx, &group, PlatformOpenAI, "", tc.requested, nil, tc.compact, OpenAIEndpointCapabilityResponses, false)
					}
					if tc.want {
						require.NoError(t, err)
						require.NotNil(t, selection)
						require.Equal(t, account.ID, selection.Account.ID)
						releaseRequestSpeedSelection(selection)
					} else {
						require.ErrorIs(t, err, ErrNoAvailableAccounts)
						require.Nil(t, selection, "unsupported actual upstream model must never yield an account to forward")
					}
				})
			}
		})
	}
}

func TestRequestSpeedSchedulerPreviousResponseCannotSwitchSupplierTier(t *testing.T) {
	const groupID int64 = 67006
	group := int64(groupID)
	ctx := WithRequestSpeed(context.Background(), groupID, "fast")
	accounts := []Account{requestSpeedSchedulerAccount(1, groupID, "ultrafast"), requestSpeedSchedulerAccount(2, groupID, "fast")}
	svc := requestSpeedSchedulerService(accounts, true)
	const responseID = "resp_fixed_ultra_previous"
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, responseID, 1, time.Hour))
	selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, responseID, "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses, false, true, false)
	require.ErrorIs(t, err, ErrRequestSpeedContinuationMismatch)
	require.Nil(t, selection, "replayable history must not carry a supplier response ID to another tier")

	scheduler := newDefaultOpenAIAccountScheduler(svc, nil)
	selection, _, err = scheduler.Select(ctx, OpenAIAccountScheduleRequest{GroupID: &group, Platform: PlatformOpenAI, PreviousResponseID: responseID, PreviousResponseCanMove: true, StickyWeighted: true, RequestedModel: "gpt-6-astra", RequiredTransport: OpenAIUpstreamTransportAny})
	require.ErrorIs(t, err, ErrRequestSpeedContinuationMismatch)
	require.Nil(t, selection)
	accountID, err := svc.getOpenAIWSStateStore().GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Equal(t, int64(1), accountID, "a rejected tier change must not delete the original response owner")
}
