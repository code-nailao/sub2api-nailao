//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sourcesLaneCall struct {
	name string
	ctx  context.Context
	args []any
}
type sourcesLaneRepo struct {
	*laneTestRepo
	calls        []sourcesLaneCall
	err          error
	settlement   *DeepSeekUsageSettlement
	identities   []DeepSeekUsageIdentity
	rejection    *UpstreamRejectionBillingResult
	reservation  *UpstreamRejectionReservation
	reservations []UpstreamRejectionReservation
	page         *UpstreamRejectionPendingPage
	userPage     *UserPendingUpstreamRejectionsPage
	imageResult  *UsageBillingApplyResult
	raw          *UsageBillingCommand
}

func (r *sourcesLaneRepo) record(name string, ctx context.Context, args ...any) {
	r.calls = append(r.calls, sourcesLaneCall{name, ctx, args})
}
func (r *sourcesLaneRepo) Apply(ctx context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.record("Apply", ctx, cmd)
	if cmd != nil {
		v := *cmd
		r.raw = &v
	}
	return nil, r.err
}
func (r *sourcesLaneRepo) BeginDeepSeekUsage(ctx context.Context, id string, key, user, account int64, deadline time.Time) error {
	r.record("BeginDeepSeekUsage", ctx, id, key, user, account, deadline)
	return r.err
}
func (r *sourcesLaneRepo) SaveDeepSeekUsage(ctx context.Context, v *DeepSeekUsageCheckpoint) error {
	r.record("SaveDeepSeekUsage", ctx, v)
	return r.err
}
func (r *sourcesLaneRepo) SettleDeepSeekUsage(ctx context.Context, id string, key int64) (*DeepSeekUsageSettlement, error) {
	r.record("SettleDeepSeekUsage", ctx, id, key)
	return r.settlement, r.err
}
func (r *sourcesLaneRepo) ListPendingDeepSeekUsage(ctx context.Context, limit int) ([]DeepSeekUsageIdentity, error) {
	r.record("ListPendingDeepSeekUsage", ctx, limit)
	return r.identities, r.err
}
func (r *sourcesLaneRepo) ReserveUpstreamRejection(ctx context.Context, v *UpstreamRejectionReserveCommand) (*UpstreamRejectionBillingResult, error) {
	r.record("ReserveUpstreamRejection", ctx, v)
	return r.rejection, r.err
}
func (r *sourcesLaneRepo) MarkUpstreamRejectionSent(ctx context.Context, id string, key int64) (*UpstreamRejectionBillingResult, error) {
	r.record("MarkUpstreamRejectionSent", ctx, id, key)
	return r.rejection, r.err
}
func (r *sourcesLaneRepo) FinalizeUpstreamRejection(ctx context.Context, v *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	r.record("FinalizeUpstreamRejection", ctx, v)
	return r.rejection, r.err
}
func (r *sourcesLaneRepo) GetUpstreamRejection(ctx context.Context, id string, key int64) (*UpstreamRejectionReservation, error) {
	r.record("GetUpstreamRejection", ctx, id, key)
	return r.reservation, r.err
}
func (r *sourcesLaneRepo) ListPendingUpstreamRejections(ctx context.Context, limit int) ([]UpstreamRejectionReservation, error) {
	r.record("ListPendingUpstreamRejections", ctx, limit)
	return r.reservations, r.err
}
func (r *sourcesLaneRepo) ListPendingUpstreamRejectionsPage(ctx context.Context, v *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	r.record("ListPendingUpstreamRejectionsPage", ctx, v)
	return r.page, r.err
}
func (r *sourcesLaneRepo) ListUnprojectedUpstreamRejectionsPage(ctx context.Context, v *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
	r.record("ListUnprojectedUpstreamRejectionsPage", ctx, v)
	return r.page, r.err
}
func (r *sourcesLaneRepo) MarkUpstreamRejectionUsageProjected(ctx context.Context, id string, key int64) error {
	r.record("MarkUpstreamRejectionUsageProjected", ctx, id, key)
	return r.err
}
func (r *sourcesLaneRepo) BindUpstreamRejectionTask(ctx context.Context, v *UpstreamRejectionTaskBinding) error {
	r.record("BindUpstreamRejectionTask", ctx, v)
	return r.err
}
func (r *sourcesLaneRepo) GetUpstreamRejectionByTask(ctx context.Context, user, key int64, id string) (*UpstreamRejectionReservation, error) {
	r.record("GetUpstreamRejectionByTask", ctx, user, key, id)
	return r.reservation, r.err
}

func (r *sourcesLaneRepo) ListUserPendingUpstreamRejections(ctx context.Context, user int64, cursor string, size int) (*UserPendingUpstreamRejectionsPage, error) {
	r.record("ListUserPendingUpstreamRejections", ctx, user, cursor, size)
	return r.userPage, r.err
}
func (r *sourcesLaneRepo) SettleSourcesImage(ctx context.Context, hold *BatchImageBalanceHoldCommand, bill *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.record("SettleSourcesImage", ctx, hold, bill)
	return r.imageResult, r.err
}
func (r *sourcesLaneRepo) ReleaseSourcesImage(ctx context.Context, hold *BatchImageBalanceHoldCommand) error {
	r.record("ReleaseSourcesImage", ctx, hold)
	return r.err
}

func TestUsageBillingLaneSourcesPrivateInterfacesPassThrough(t *testing.T) {
	sentinel := errors.New("durable transaction outcome unknown")
	repo := &sourcesLaneRepo{laneTestRepo: newLaneTestRepo(), err: sentinel, settlement: &DeepSeekUsageSettlement{}, identities: []DeepSeekUsageIdentity{{RequestID: "pending"}}, rejection: &UpstreamRejectionBillingResult{}, reservation: &UpstreamRejectionReservation{}, reservations: []UpstreamRejectionReservation{{}}, page: &UpstreamRejectionPendingPage{}}
	c := NewUsageBillingLaneCombiner(repo)
	defer c.Stop()
	repo.userPage = &UserPendingUpstreamRejectionsPage{}
	repo.imageResult = &UsageBillingApplyResult{Applied: true}
	wired := ProvideUsageBillingRepository(c)
	journal, ok := wired.(DeepSeekUsageJournal)
	require.True(t, ok)
	rejection, ok := wired.(UpstreamRejectionBillingRepository)
	require.True(t, ok)
	images, ok := wired.(SourcesImageLedger)
	require.True(t, ok)
	userPending, ok := wired.(UserPendingUpstreamRejectionReader)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // preserve cancellation rather than silently detach journal calls
	deadline := time.Unix(123, 456)
	checkpoint := &DeepSeekUsageCheckpoint{Revision: 7}
	reserve := &UpstreamRejectionReserveCommand{}
	finalize := &UpstreamRejectionFinalizeCommand{}
	query := &UpstreamRejectionPendingQuery{Limit: 19, TasksOnly: true}
	binding := &UpstreamRejectionTaskBinding{}
	check := func(name string, args ...any) {
		t.Helper()
		require.Len(t, repo.calls, 1)
		require.Equal(t, name, repo.calls[0].name)
		require.Same(t, ctx, repo.calls[0].ctx)
		require.Equal(t, args, repo.calls[0].args)
		repo.calls = nil
	}
	require.ErrorIs(t, journal.BeginDeepSeekUsage(ctx, "begin", 2, 3, 4, deadline), sentinel)
	check("BeginDeepSeekUsage", "begin", int64(2), int64(3), int64(4), deadline)
	require.ErrorIs(t, journal.SaveDeepSeekUsage(ctx, checkpoint), sentinel)
	check("SaveDeepSeekUsage", checkpoint)
	settlement, err := journal.SettleDeepSeekUsage(ctx, "settle", 2)
	require.Same(t, repo.settlement, settlement)
	require.ErrorIs(t, err, sentinel)
	check("SettleDeepSeekUsage", "settle", int64(2))
	identities, err := journal.ListPendingDeepSeekUsage(ctx, 17)
	require.Equal(t, repo.identities, identities)
	require.ErrorIs(t, err, sentinel)
	check("ListPendingDeepSeekUsage", 17)
	res, err := rejection.ReserveUpstreamRejection(ctx, reserve)
	require.Same(t, repo.rejection, res)
	require.ErrorIs(t, err, sentinel)
	check("ReserveUpstreamRejection", reserve)
	res, err = rejection.MarkUpstreamRejectionSent(ctx, "sent", 2)
	require.Same(t, repo.rejection, res)
	require.ErrorIs(t, err, sentinel)
	check("MarkUpstreamRejectionSent", "sent", int64(2))
	res, err = rejection.FinalizeUpstreamRejection(ctx, finalize)
	require.Same(t, repo.rejection, res)
	require.ErrorIs(t, err, sentinel)
	check("FinalizeUpstreamRejection", finalize)
	reservation, err := rejection.GetUpstreamRejection(ctx, "get", 2)
	require.Same(t, repo.reservation, reservation)
	require.ErrorIs(t, err, sentinel)
	check("GetUpstreamRejection", "get", int64(2))
	reservations, err := rejection.ListPendingUpstreamRejections(ctx, 18)
	require.Equal(t, repo.reservations, reservations)
	require.ErrorIs(t, err, sentinel)
	check("ListPendingUpstreamRejections", 18)
	page, err := rejection.ListPendingUpstreamRejectionsPage(ctx, query)
	require.Same(t, repo.page, page)
	require.ErrorIs(t, err, sentinel)
	check("ListPendingUpstreamRejectionsPage", query)
	page, err = rejection.ListUnprojectedUpstreamRejectionsPage(ctx, query)
	require.Same(t, repo.page, page)
	require.ErrorIs(t, err, sentinel)
	check("ListUnprojectedUpstreamRejectionsPage", query)
	require.ErrorIs(t, rejection.MarkUpstreamRejectionUsageProjected(ctx, "projected", 2), sentinel)
	check("MarkUpstreamRejectionUsageProjected", "projected", int64(2))
	require.ErrorIs(t, rejection.BindUpstreamRejectionTask(ctx, binding), sentinel)
	check("BindUpstreamRejectionTask", binding)
	reservation, err = rejection.GetUpstreamRejectionByTask(ctx, 3, 2, "task")
	require.Same(t, repo.reservation, reservation)
	require.ErrorIs(t, err, sentinel)
	check("GetUpstreamRejectionByTask", int64(3), int64(2), "task")
	userPage, err := userPending.ListUserPendingUpstreamRejections(ctx, 3, "opaque cursor", 17)
	require.Same(t, repo.userPage, userPage)
	require.ErrorIs(t, err, sentinel)
	check("ListUserPendingUpstreamRejections", int64(3), "opaque cursor", 17)
	// The repository must receive the original hold and bill before normalization.
	hold := &BatchImageBalanceHoldCommand{BatchID: " original task ", HoldAmount: -1e-12}
	bill := &UsageBillingCommand{RequestID: " original request ", BalanceCost: -1e-12}
	wantHold, wantBill := *hold, *bill
	imageResult, err := images.SettleSourcesImage(ctx, hold, bill)
	require.Same(t, repo.imageResult, imageResult)
	require.ErrorIs(t, err, sentinel)
	check("SettleSourcesImage", hold, bill)
	require.ErrorIs(t, images.ReleaseSourcesImage(ctx, hold), sentinel)
	check("ReleaseSourcesImage", hold)
	require.Equal(t, wantHold, *hold)
	require.Equal(t, wantBill, *bill)
	require.Empty(t, repo.batchCalls)
	require.Equal(t, UsageBillingLaneStats{}, c.Stats())
}

func TestUsageBillingLaneSourcesUnsupportedRepositoriesFailClosed(t *testing.T) {
	for _, c := range []*UsageBillingLaneCombiner{nil, NewUsageBillingLaneCombiner(newLaneTestRepo())} {
		ctx := context.Background()
		require.ErrorIs(t, c.BeginDeepSeekUsage(ctx, "x", 1, 2, 3, time.Now()), errUsageBillingLaneDeepSeekJournalUnavailable)
		require.ErrorIs(t, c.SaveDeepSeekUsage(ctx, nil), errUsageBillingLaneDeepSeekJournalUnavailable)
		v, err := c.SettleDeepSeekUsage(ctx, "x", 1)
		require.Nil(t, v)
		require.ErrorIs(t, err, errUsageBillingLaneDeepSeekJournalUnavailable)
		ids, err := c.ListPendingDeepSeekUsage(ctx, 1)
		require.Nil(t, ids)
		require.ErrorIs(t, err, errUsageBillingLaneDeepSeekJournalUnavailable)
		r, err := c.ReserveUpstreamRejection(ctx, nil)
		require.Nil(t, r)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		r, err = c.MarkUpstreamRejectionSent(ctx, "x", 1)
		require.Nil(t, r)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		r, err = c.FinalizeUpstreamRejection(ctx, nil)
		require.Nil(t, r)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		hold, err := c.GetUpstreamRejection(ctx, "x", 1)
		require.Nil(t, hold)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		holds, err := c.ListPendingUpstreamRejections(ctx, 1)
		require.Nil(t, holds)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		page, err := c.ListPendingUpstreamRejectionsPage(ctx, nil)
		require.Nil(t, page)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		page, err = c.ListUnprojectedUpstreamRejectionsPage(ctx, nil)
		require.Nil(t, page)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		require.ErrorIs(t, c.MarkUpstreamRejectionUsageProjected(ctx, "x", 1), errUsageBillingLaneRejectionRepositoryUnavailable)
		require.ErrorIs(t, c.BindUpstreamRejectionTask(ctx, nil), errUsageBillingLaneRejectionRepositoryUnavailable)
		hold, err = c.GetUpstreamRejectionByTask(ctx, 2, 1, "x")
		require.Nil(t, hold)
		require.ErrorIs(t, err, errUsageBillingLaneRejectionRepositoryUnavailable)
		imageResult, err := c.SettleSourcesImage(ctx, nil, nil)
		require.Nil(t, imageResult)
		require.ErrorIs(t, err, errUsageBillingLaneImageLedgerUnavailable)
		require.ErrorIs(t, c.ReleaseSourcesImage(ctx, nil), errUsageBillingLaneImageLedgerUnavailable)
		userPage, err := c.ListUserPendingUpstreamRejections(ctx, 2, "cursor", 7)
		require.Nil(t, userPage)
		require.ErrorIs(t, err, errUsageBillingLaneUserPendingReaderUnavailable)
		if c != nil {
			c.Stop()
		}
	}
}

func TestUsageBillingLaneSourcesPrivateCommandsPreserveRawTransaction(t *testing.T) {
	cases := []struct {
		name string
		set  func(*UsageBillingCommand)
	}{
		{"rejection hold", func(c *UsageBillingCommand) { c.UpstreamRejectionReservationID = "hold" }},
		{"tool fingerprint", func(c *UsageBillingCommand) { c.DeepSeekToolFingerprint = "tool" }},
		{"tool account", func(c *UsageBillingCommand) { c.ToolAccountID = 3 }},
		{"tool cost", func(c *UsageBillingCommand) { c.ToolAccountQuotaCost = -1e-12 }},
		{"tool daily", func(c *UsageBillingCommand) { c.ToolAccountDailyQuota = true }},
		{"tool weekly", func(c *UsageBillingCommand) { c.ToolAccountWeeklyQuota = true }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			errInvalid := errors.New("raw amount rejected")
			repo := &sourcesLaneRepo{laneTestRepo: newLaneTestRepo(), err: errInvalid}
			c := NewUsageBillingLaneCombiner(repo)
			defer c.Stop()
			cmd := &UsageBillingCommand{RequestID: " raw ", UserID: 1, APIKeyID: 2, BalanceCost: -1e-12, RequestFingerprint: ""}
			tt.set(cmd)
			want := *cmd
			result, err := c.Apply(context.Background(), cmd)
			require.Nil(t, result)
			require.ErrorIs(t, err, errInvalid)
			require.Equal(t, want, *repo.raw)
			require.Equal(t, want, *cmd)
			require.Len(t, repo.calls, 1)
			require.Equal(t, "Apply", repo.calls[0].name)
			require.Empty(t, repo.batchCalls)
			require.Equal(t, UsageBillingLaneStats{}, c.Stats())
		})
	}
	require.False(t, HasPrivateUsageBillingEffects(nil))
	require.False(t, HasPrivateUsageBillingEffects(&UsageBillingCommand{}))
}
