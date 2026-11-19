package service

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type rejectionFacadeRepo struct {
	UsageBillingRepository
	UpstreamRejectionBillingRepository
	row                                              *UpstreamRejectionReservation
	reserveCalls, markCalls, finalizeCalls, captures int
	markErr, finalizeErr                             error
	lastCtxErr                                       error
}

func (r *rejectionFacadeRepo) ReserveUpstreamRejection(_ context.Context, c *UpstreamRejectionReserveCommand) (*UpstreamRejectionBillingResult, error) {
	r.reserveCalls++
	r.row = &UpstreamRejectionReservation{UpstreamRejectionReserveCommand: *c, State: UpstreamRejectionReserved, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	return &UpstreamRejectionBillingResult{Applied: true, Reservation: r.row}, nil
}
func (r *rejectionFacadeRepo) MarkUpstreamRejectionSent(_ context.Context, _ string, _ int64) (*UpstreamRejectionBillingResult, error) {
	r.markCalls++
	if r.markErr != nil {
		return nil, r.markErr
	}
	r.row.State = UpstreamRejectionSent
	return &UpstreamRejectionBillingResult{Applied: true, Reservation: r.row}, nil
}
func (r *rejectionFacadeRepo) FinalizeUpstreamRejection(ctx context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	r.finalizeCalls++
	r.lastCtxErr = ctx.Err()
	if r.finalizeErr != nil {
		return nil, r.finalizeErr
	}
	applied := r.row.State != c.Outcome
	if applied && c.Outcome == UpstreamRejectionRejected {
		r.captures++
		r.row.Evidence = c.Evidence
	}
	r.row.State = c.Outcome
	return &UpstreamRejectionBillingResult{Applied: applied, Reservation: r.row}, nil
}
func (r *rejectionFacadeRepo) MarkUpstreamRejectionUsageProjected(_ context.Context, _ string, _ int64) error {
	now := time.Now()
	r.row.UsageProjectedAt = &now
	return nil
}
func (r *rejectionFacadeRepo) GetUpstreamRejection(_ context.Context, _ string, _ int64) (*UpstreamRejectionReservation, error) {
	return r.row, nil
}

func rejectionFacadeFixture() (*OpenAIGatewayService, *rejectionFacadeRepo, *APIKey, *Account) {
	repo := &rejectionFacadeRepo{}
	group := &Group{ID: 4, Platform: PlatformGrok, SubscriptionType: SubscriptionTypeStandard, UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: 0.1, RateMultiplier: 7}
	key := &APIKey{ID: 2, UserID: 1, User: &User{ID: 1}, GroupID: &group.ID, Group: group}
	account := &Account{ID: 3, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://supplier.invalid", "api_key": "fixture-only"}, Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1, "upstream_rejection_cost": 0.06}}
	return &OpenAIGatewayService{usageBillingRepo: repo}, repo, key, account
}
func beginMarkedRejection(t *testing.T) (*OpenAIGatewayService, *rejectionFacadeRepo, *APIKey, *Account, *UpstreamRejectionBillingAttempt) {
	t.Helper()
	s, r, k, a := rejectionFacadeFixture()
	attempt, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte(`{"model":"grok-4.7"}`))
	require.NoError(t, err)
	require.NoError(t, s.MarkUpstreamRejectionBillingSent(WithUpstreamRejectionBillingAttempt(context.Background(), attempt)))
	return s, r, k, a, attempt
}
func TestUpstreamRejectionFacadeAdmissionAndActualSend(t *testing.T) {
	s, r, k, a := rejectionFacadeFixture()
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "reuse-this-client-id")
	first, err := s.BeginUpstreamRejectionBilling(ctx, k, a, "grok-4.7", []byte(`{"model":"grok-4.7"}`))
	require.NoError(t, err)
	require.Zero(t, r.markCalls, "reservation is not a network send")
	require.Equal(t, 0.1, first.Reservation.Price)
	require.NotContains(t, first.Reservation.RequestID, "reuse-this")
	require.NoError(t, s.MarkUpstreamRejectionBillingSent(WithUpstreamRejectionBillingAttempt(ctx, first)))
	require.ErrorIs(t, s.MarkUpstreamRejectionBillingSent(WithUpstreamRejectionBillingAttempt(ctx, first)), ErrUpstreamRejectionConflict)
	require.Equal(t, 1, r.markCalls)
	second, err := s.BeginUpstreamRejectionBilling(ctx, k, a, "grok-4.7", []byte(`{"model":"grok-4.7"}`))
	require.NoError(t, err)
	require.NotEqual(t, first.Reservation.RequestID, second.Reservation.RequestID)
}
func TestUpstreamRejectionFacadeFailsClosedForMisconfiguration(t *testing.T) {
	cases := map[string]func(*APIKey, *Account){"oauth": func(k *APIKey, a *Account) { a.Type = AccountTypeOAuth }, "wrong platform": func(k *APIKey, a *Account) { a.Platform = PlatformOpenAI }, "missing profile": func(k *APIKey, a *Account) { a.Extra = nil }, "subscription": func(k *APIKey, a *Account) { k.Group.SubscriptionType = "subscription" }, "owner mismatch": func(k *APIKey, a *Account) { k.User.ID = 9 }, "zero price": func(k *APIKey, a *Account) { k.Group.UpstreamRejectionPrice = 0 }, "bad cost": func(k *APIKey, a *Account) { a.Extra["upstream_rejection_cost"] = math.NaN() }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, r, k, a := rejectionFacadeFixture()
			mutate(k, a)
			_, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte("{}"))
			require.Error(t, err)
			require.Zero(t, r.reserveCalls)
		})
	}
	s, r, k, a := rejectionFacadeFixture()
	k.Group.UpstreamRejectionBillingEnabled = false
	a.Extra = nil
	attempt, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte("{}"))
	require.NoError(t, err)
	require.Nil(t, attempt)
	require.Zero(t, r.reserveCalls)
	k.Group.UpstreamRejectionBillingEnabled = true
	a.Extra = map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1}
	s.usageBillingRepo = nil
	_, err = s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte("{}"))
	require.ErrorIs(t, err, ErrUpstreamRejectionBillingUnavailable)
}
func TestUpstreamRejectionFacadeLocalFailureReleasesWithDetachedContext(t *testing.T) {
	s, r, k, a := rejectionFacadeFixture()
	attempt, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte("{}"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	localErr := errors.New("local request validation failed")
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(ctx, attempt, nil, localErr), localErr)
	require.Equal(t, UpstreamRejectionReleased, r.row.State)
	require.NoError(t, r.lastCtxErr)
	s, r, k, a = rejectionFacadeFixture()
	attempt, err = s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte("{}"))
	require.NoError(t, err)
	r.markErr = context.Canceled
	require.ErrorIs(t, s.MarkUpstreamRejectionBillingSent(WithUpstreamRejectionBillingAttempt(ctx, attempt)), context.Canceled)
	require.Equal(t, UpstreamRejectionReleased, r.row.State)
	require.NoError(t, r.lastCtxErr)
}
func TestUpstreamRejectionFacadeIncompleteResultsStayUnknown(t *testing.T) {
	cases := []struct {
		name   string
		result *OpenAIForwardResult
		err    error
	}{
		{"empty success", &OpenAIForwardResult{}, nil},
		{"partial usage EOF", &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 10, OutputTokens: 2}}, io.ErrUnexpectedEOF},
		{"stream no terminal no error", &OpenAIForwardResult{Stream: true, Usage: OpenAIUsage{InputTokens: 10}}, nil},
		{"nil transport", nil, io.EOF},
		{"five hundred", nil, &UpstreamFailoverError{StatusCode: 500}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, r, k, a, attempt := beginMarkedRejection(t)
			require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, c.result, c.err), ErrUpstreamRejectionOutcomeUnknown)
			require.Equal(t, UpstreamRejectionUnknown, r.row.State)
			require.Zero(t, r.captures)
			if c.result != nil {
				c.result.Model = "grok-4.7"
				require.ErrorIs(t, s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: c.result, APIKey: k, User: k.User, Account: a}), ErrUpstreamRejectionOutcomeUnknown)
			}
		})
	}
}
func TestUpstreamRejectionFacadeConfirmedNormalAndNonbillableError(t *testing.T) {
	s, r, _, _, attempt := beginMarkedRejection(t)
	res := &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "chat.completed", Usage: OpenAIUsage{InputTokens: 10}}
	require.NoError(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, res, nil))
	require.Equal(t, UpstreamRejectionSent, r.row.State)
	require.Equal(t, attempt.Reservation.RequestID, res.RequestID)
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		s, r, _, _, attempt = beginMarkedRejection(t)
		e := &UpstreamFailoverError{StatusCode: status}
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, e), e)
		require.Equal(t, UpstreamRejectionReleased, r.row.State)
	}
}
func TestUpstreamRejectionFacadePersistsRejectionBeforeUsageProjection(t *testing.T) {
	s, r, k, a, attempt := beginMarkedRejection(t)
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	s.usageLogRepo = logs
	ev := &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileGrokContentV1, RuleID: "text.refusal", RuleVersion: UpstreamRejectionRuleVersion(), HTTPStatus: 200, ResponseHash: strings.Repeat("a", 64)}
	res := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejection: ev, Usage: OpenAIUsage{InputTokens: 20, OutputTokens: 4, CacheReadInputTokens: 5}}
	require.NoError(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, res, nil))
	require.Equal(t, 1, r.captures)
	require.Zero(t, logs.calls)
	// Mutating live prices and poll tokens cannot change an already captured bill.
	k.Group.UpstreamRejectionPrice = 2
	k.Group.RateMultiplier = 99
	a.Extra["upstream_rejection_cost"] = 9.
	res.Usage.OutputTokens = 999
	require.NoError(t, s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: res, APIKey: k, User: k.User, Account: a}))
	require.Equal(t, 1, r.captures)
	require.Equal(t, 0.1, logs.lastLog.ActualCost)
	require.Equal(t, 0.1, logs.lastLog.TotalCost)
	require.Equal(t, 0.06, *logs.lastLog.AccountStatsCost)
	require.Equal(t, 4, logs.lastLog.OutputTokens)
	require.Equal(t, 1., logs.lastLog.RateMultiplier)
	require.Equal(t, "upstream_rejection", logs.lastLog.BillingSnapshot.ChargeKind)
	logs.err = errors.New("projection temporarily unavailable")
	require.Error(t, s.ProjectUpstreamRejectionUsage(context.Background(), r.row))
	logs.err = nil
	require.NoError(t, s.ProjectUpstreamRejectionUsage(context.Background(), r.row))
	require.Equal(t, 1, r.captures, "projection recovery must never debit")
}
func TestUpstreamRejectionFacadeRestoresNormalBindingAndProtectsOwnership(t *testing.T) {
	s, r, k, a, attempt := beginMarkedRejection(t)
	result := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejectionReservationID: attempt.Reservation.RequestID, Usage: OpenAIUsage{InputTokens: 1}}
	in := &OpenAIRecordUsageInput{Result: result, APIKey: k, User: k.User, Account: a, RequestPayloadHash: "changed transport body"}
	_, err := s.rejectionReservationForUsage(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, r.row.RequestPayloadHash, in.RequestPayloadHash)
	require.Equal(t, r.row.RequestID, result.RequestID)
	in.User = &User{ID: 99}
	_, err = s.rejectionReservationForUsage(context.Background(), in)
	require.ErrorIs(t, err, ErrUpstreamRejectionConflict)
}
func TestUpstreamRejectionNormalCommandKeepsServerHoldIdentity(t *testing.T) {
	s, _, k, a, attempt := beginMarkedRejection(t)
	_ = s
	p := &postUsageBillingParams{Cost: &CostBreakdown{TotalCost: 2, ActualCost: 0.26}, User: k.User, APIKey: k, Account: a, UpstreamRejectionReservationID: attempt.Reservation.RequestID, RequestPayloadHash: attempt.Reservation.RequestPayloadHash}
	cmd := buildUsageBillingCommand(attempt.Reservation.RequestID, &UsageLog{Model: "grok-4.7"}, p)
	require.Equal(t, attempt.Reservation.RequestID, cmd.UpstreamRejectionReservationID)
	require.Equal(t, attempt.Reservation.RequestPayloadHash, cmd.RequestPayloadHash)
}

func TestUpstreamRejectionFacadeObservedPlainHTTPFailureIsNotUnknown(t *testing.T) {
	s, r, _, _, attempt := beginMarkedRejection(t)
	original := errors.New("provider parameter error")
	wrapped := &UpstreamRejectionHTTPError{StatusCode: 400, Cause: original}
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, wrapped), original)
	require.Equal(t, UpstreamRejectionReleased, r.row.State)
}

func TestUpstreamRejectionFacadeMappedModelKeepsRequestedBinding(t *testing.T) {
	s, r, k, a, attempt := beginMarkedRejection(t)
	result := &OpenAIForwardResult{Model: "grok-4.6", UpstreamModel: "grok-4.6", UpstreamRejectionReservationID: attempt.Reservation.RequestID, Usage: OpenAIUsage{InputTokens: 1}}
	in := &OpenAIRecordUsageInput{Result: result, APIKey: k, User: k.User, Account: a, ChannelUsageFields: ChannelUsageFields{OriginalModel: "grok-4.7"}}
	_, err := s.rejectionReservationForUsage(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, r.row.RequestPayloadHash, in.RequestPayloadHash)
	in.OriginalModel = "grok-4.5"
	_, err = s.rejectionReservationForUsage(context.Background(), in)
	require.ErrorIs(t, err, ErrUpstreamRejectionConflict)
}
func TestUpstreamRejectionFacadeRejectsUnsupportedAsyncTextBeforeReserve(t *testing.T) {
	for _, body := range []string{`{"background":true}`, `{"deferred":true}`, `{"background":"true"}`} {
		s, r, k, a := rejectionFacadeFixture()
		_, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte(body))
		require.Error(t, err)
		require.Zero(t, r.reserveCalls)
	}
}

func TestUpstreamRejectionFacadeConcurrentRefusalPollUsesWinningReceipt(t *testing.T) {
	s, r, k, a, attempt := beginMarkedRejection(t)
	stale := *r.row
	r.row.State = UpstreamRejectionRejected
	r.row.Evidence = UpstreamRejectionBillingEvidence{RuleID: "winner", RuleVersion: UpstreamRejectionRuleVersion(), OriginalStatus: 403, OutputTokens: 7}
	r.finalizeErr = ErrUpstreamRejectionConflict
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	s.usageLogRepo = logs
	result := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejectionReservationID: attempt.Reservation.RequestID, UpstreamRejection: &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileGrokContentV1, RuleVersion: UpstreamRejectionRuleVersion(), RuleID: "later-observer", HTTPStatus: 403}}
	require.NoError(t, s.recordUpstreamRejectionUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: k, User: k.User, Account: a}, &stale))
	require.Equal(t, "winner", logs.lastLog.BillingSnapshot.RejectionRuleID)
	require.Equal(t, 7, logs.lastLog.OutputTokens)
	require.Zero(t, r.captures)
}
