//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type grokRejectionMediaStore struct {
	UsageBillingRepository
	reservation *UpstreamRejectionReservation
	lookupErr   error
	bound       *UpstreamRejectionTaskBinding
	finalized   *UpstreamRejectionFinalizeCommand
}

func (s *grokRejectionMediaStore) ReserveUpstreamRejection(context.Context, *UpstreamRejectionReserveCommand) (*UpstreamRejectionBillingResult, error) {
	return nil, nil
}
func (s *grokRejectionMediaStore) MarkUpstreamRejectionSent(context.Context, string, int64) (*UpstreamRejectionBillingResult, error) {
	return nil, nil
}
func (s *grokRejectionMediaStore) FinalizeUpstreamRejection(_ context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	s.finalized = c
	return &UpstreamRejectionBillingResult{}, nil
}
func (s *grokRejectionMediaStore) GetUpstreamRejection(context.Context, string, int64) (*UpstreamRejectionReservation, error) {
	return s.reservation, s.lookupErr
}
func (s *grokRejectionMediaStore) ListPendingUpstreamRejections(context.Context, int) ([]UpstreamRejectionReservation, error) {
	return nil, nil
}
func (s *grokRejectionMediaStore) ListPendingUpstreamRejectionsPage(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	return &UpstreamRejectionPendingPage{}, nil
}
func (s *grokRejectionMediaStore) ListUnprojectedUpstreamRejectionsPage(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	return &UpstreamRejectionPendingPage{}, nil
}
func (s *grokRejectionMediaStore) MarkUpstreamRejectionUsageProjected(context.Context, string, int64) error {
	return nil
}
func (s *grokRejectionMediaStore) BindUpstreamRejectionTask(_ context.Context, b *UpstreamRejectionTaskBinding) error {
	s.bound = b
	return nil
}
func (s *grokRejectionMediaStore) GetUpstreamRejectionByTask(_ context.Context, u, k int64, task string) (*UpstreamRejectionReservation, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.reservation == nil || s.reservation.UserID != u || s.reservation.APIKeyID != k || s.reservation.Task.TaskID != task {
		return nil, ErrUpstreamRejectionNotFound
	}
	return s.reservation, nil
}

func grokRejectionMediaReservation() *UpstreamRejectionReservation {
	identity, _ := UpstreamRejectionIdentityHash(grokMediaContentTestAccount())
	return &UpstreamRejectionReservation{
		UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{RequestID: NewUpstreamRejectionRequestID(), UserID: 2, APIKeyID: 3, GroupID: 4, AccountID: 9, Model: "grok-imagine-video", ProviderProfile: "grok_content_rejection_v1", Price: .1, UpstreamCost: .1, UpstreamCurrency: "USD", UpstreamIdentityHash: identity},
		State:                           UpstreamRejectionSent, CreatedAt: time.Now().UTC().Add(-time.Minute),
		Task: &UpstreamRejectionTaskBinding{TaskID: "task-owned", DurationSeconds: 6, Resolution: "720p", OriginalModel: "grok-imagine-video", UpstreamModel: "grok-imagine-video"},
	}
}

func TestGrokRejectionTaskSurvivesRedisLossAndKeepsOwner(t *testing.T) {
	r := grokRejectionMediaReservation()
	store := &grokRejectionMediaStore{reservation: r}
	s := &OpenAIGatewayService{usageBillingRepo: store}
	group := int64(4)
	account, err := s.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "task-owned", 2, 3)
	require.NoError(t, err)
	require.Equal(t, int64(9), account)
	_, err = s.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "task-owned", 2, 33)
	require.Error(t, err)
	group = 5
	account, err = s.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "task-owned", 2, 3)
	require.NoError(t, err)
	require.Equal(t, int64(9), account, "rebinding the same key must not lose its admitted task")
	pending, err := s.LoadGrokVideoPendingBilling(context.Background(), "task-owned", 2, 3)
	require.NoError(t, err)
	require.Equal(t, r.RequestID, pending.UpstreamRejectionReservationID)
	require.Equal(t, "720p", pending.VideoResolution)
	require.Equal(t, 6, pending.VideoDurationSeconds)
	claimed, err := s.ClaimGrokVideoBilling(context.Background(), "task-owned", 2, 3)
	require.NoError(t, err)
	require.True(t, claimed)
	r.State = UpstreamRejectionRejected
	claimed, err = s.ClaimGrokVideoBilling(context.Background(), "task-owned", 2, 3)
	require.NoError(t, err)
	require.False(t, claimed)
	store.lookupErr = errors.New("database down")
	_, err = s.LoadGrokVideoPendingBilling(context.Background(), "task-owned", 2, 3)
	require.EqualError(t, err, "database down")
}

func TestGrokRejectionCreatePersistsBindingWithoutRedis(t *testing.T) {
	store := &grokRejectionMediaStore{}
	s := &OpenAIGatewayService{usageBillingRepo: store}
	id := NewUpstreamRejectionRequestID()
	base, final := .08, .15
	err := s.StoreGrokVideoPendingBilling(context.Background(), "task-owned", 2, 3, GrokVideoPendingBilling{Model: "grok-imagine-video", UpstreamRejectionReservationID: id, VideoDurationSeconds: 6, VideoResolution: "720p", VideoBasePricePerSecond: &base, VideoFinalPricePerSecond: &final})
	require.NoError(t, err)
	require.Equal(t, id, store.bound.RequestID)
	require.Equal(t, "task-owned", store.bound.TaskID)
	require.Equal(t, int64(3), store.bound.APIKeyID)
	require.Equal(t, base, *store.bound.VideoBasePricePerSecond)
	require.Equal(t, final, *store.bound.VideoFinalPricePerSecond)
}

func TestGrokRejectionVideoNormalPriceUsesCreateSnapshotAfterGroupAndKeyChanges(t *testing.T) {
	r := grokRejectionMediaReservation()
	base, final, changedPrice := .08, .15, 99.0
	r.Task.VideoBasePricePerSecond, r.Task.VideoFinalPricePerSecond = &base, &final
	r.RequestPayloadHash = HashUsageRequestPayload([]byte(`{"create":"body"}`))
	changedGroupID := int64(88)
	key := &APIKey{ID: 3, UserID: 2, User: &User{ID: 2}, GroupID: &changedGroupID,
		Group: &Group{ID: changedGroupID, Platform: PlatformGrok, RateMultiplier: 200, VideoPrice720P: &changedPrice}}
	billingKey, err := RestoreGrokVideoRejectionBillingKey(key, r)
	require.NoError(t, err)
	require.Equal(t, r.GroupID, *billingKey.GroupID)
	require.Equal(t, changedGroupID, *key.GroupID)
	require.Equal(t, 200.0, key.Group.RateMultiplier, "authenticated cached key must not be mutated")
	store := &grokRejectionMediaStore{reservation: r}
	s := &OpenAIGatewayService{usageBillingRepo: store}
	pending, err := s.LoadGrokVideoPendingBilling(context.Background(), "task-owned", 2, 3)
	require.NoError(t, err)
	result := ExtractGrokVideoBillingFromStatusBody([]byte(`{"status":"done","video":{"url":"https://example.invalid/video","duration":3}}`), pending, "task-owned")
	require.NotNil(t, result)
	require.Equal(t, base, *result.VideoBasePricePerSecond)
	require.Equal(t, final, *result.VideoFinalPricePerSecond)
	input := &OpenAIRecordUsageInput{Result: result, APIKey: billingKey, User: key.User, Account: &Account{ID: 9}, RequestPayloadHash: HashUsageRequestPayload([]byte("poll-specific-hash"))}
	_, err = s.rejectionReservationForUsage(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, r.RequestID, result.RequestID)
	require.Equal(t, r.RequestPayloadHash, input.RequestPayloadHash)
	cost := s.calculateOpenAIVideoCost(context.Background(), r.Model, key, result, 999)
	require.InDelta(t, .24, cost.TotalCost, 1e-12)
	require.InDelta(t, .45, cost.ActualCost, 1e-12)
}

func TestGrokRejectionBoundTaskSelectionDoesNotRequireRedisStickyOrNewGenerationScheduling(t *testing.T) {
	r := grokRejectionMediaReservation()
	account := grokMediaContentTestAccount()
	account.Schedulable = false
	s := &OpenAIGatewayService{accountRepo: &grokRejectionPollAccountRepo{account: account}, cfg: &config.Config{}}
	selected, decision, err := s.SelectGrokVideoRejectionTaskAccount(context.Background(), r)
	require.NoError(t, err)
	require.Equal(t, r.AccountID, selected.Account.ID)
	require.NotNil(t, selected.WaitPlan, "lookup still requires concurrency admission")
	require.True(t, decision.StickySessionHit)
	require.False(t, account.Schedulable)
}

type grokRejectionPollAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *grokRejectionPollAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func TestGrokRejectionBackgroundPollingDoesNotGuessFeeFromFailedHTTP200(t *testing.T) {
	for _, status := range []string{"pending", "failed", "expired"} {
		t.Run(status, func(t *testing.T) {
			r := grokRejectionMediaReservation()
			store := &grokRejectionMediaStore{reservation: r}
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"status":"` + status + `","error":{"message":"generated video rejected by content moderation"}}`)}
			s := &OpenAIGatewayService{cfg: &config.Config{}, usageBillingRepo: store, accountRepo: &grokRejectionPollAccountRepo{account: grokMediaContentTestAccount()}, httpUpstream: upstream}
			key := &APIKey{ID: 3, UserID: 2, User: &User{ID: 2}, Group: &Group{ID: 4}}
			settled, err := s.ReconcileGrokVideoRejection(context.Background(), r, key, nil)
			require.NoError(t, err)
			require.False(t, settled)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "GET", upstream.requests[0].Method)
			if status == "pending" {
				require.Nil(t, store.finalized)
			} else {
				require.Equal(t, UpstreamRejectionUnknown, store.finalized.Outcome)
			}
		})
	}
}

func TestGrokRejectionBackgroundPollingSkipsSettledAndRejectsWrongOwner(t *testing.T) {
	r := grokRejectionMediaReservation()
	r.State = UpstreamRejectionRejected
	s := &OpenAIGatewayService{}
	key := &APIKey{ID: 3, UserID: 2, User: &User{ID: 2}, Group: &Group{ID: 4}}
	settled, err := s.ReconcileGrokVideoRejection(context.Background(), r, key, nil)
	require.NoError(t, err)
	require.True(t, settled)
	key.UserID = 7
	_, err = s.ReconcileGrokVideoRejection(context.Background(), r, key, nil)
	require.ErrorIs(t, err, ErrUpstreamRejectionInvalid)
}

func TestGrokRejectionBackgroundPollingStopsBeforeChangedUpstreamIdentity(t *testing.T) {
	for _, field := range []string{"base_url", "api_key", "account_type"} {
		t.Run(field, func(t *testing.T) {
			r := grokRejectionMediaReservation()
			store := &grokRejectionMediaStore{reservation: r}
			account := grokMediaContentTestAccount()
			account.Credentials[field] = "changed"
			if field == "account_type" {
				account.Type = AccountTypeOAuth
			}
			if field == "base_url" {
				account.Credentials[field] = "https://other.example/v1"
			}
			upstream := &grokMediaContentUpstreamStub{}
			s := &OpenAIGatewayService{usageBillingRepo: store, accountRepo: &grokRejectionPollAccountRepo{account: account}, httpUpstream: upstream}
			key := &APIKey{ID: 3, UserID: 2, User: &User{ID: 2}, Group: &Group{ID: 4}}
			_, err := s.ReconcileGrokVideoRejection(context.Background(), r, key, nil)
			require.ErrorIs(t, err, ErrUpstreamRejectionConflict)
			require.Empty(t, upstream.requests, "old task must never be queried against new credentials or target")
			require.NotNil(t, store.finalized)
			require.Equal(t, UpstreamRejectionUnknown, store.finalized.Outcome)
		})
	}
}

func TestGrokRejectionBackgroundPollingAllowsModelListMetadataEdit(t *testing.T) {
	r := grokRejectionMediaReservation()
	store := &grokRejectionMediaStore{reservation: r}
	account := grokMediaContentTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"grok-imagine-video": "grok-imagine-video"}
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"status":"pending"}`)}
	s := &OpenAIGatewayService{usageBillingRepo: store, cfg: &config.Config{}, accountRepo: &grokRejectionPollAccountRepo{account: account}, httpUpstream: upstream}
	key := &APIKey{ID: 3, UserID: 2, User: &User{ID: 2}, Group: &Group{ID: 4}}
	settled, err := s.ReconcileGrokVideoRejection(context.Background(), r, key, nil)
	require.NoError(t, err)
	require.False(t, settled)
	require.Len(t, upstream.requests, 1)
	require.Nil(t, store.finalized)
}
