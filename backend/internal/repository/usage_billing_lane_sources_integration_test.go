//go:build integration

package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests wrap the real PostgreSQL repository in the same combiner as Wire.
// Checking the underlying repository alone would miss lost optional interfaces
// and private commands being sent through the native batch transaction.
func newSourcesBillingLane(t *testing.T) *service.UsageBillingLaneCombiner {
	t.Helper()
	lane := service.NewUsageBillingLaneCombiner(NewUsageBillingRepository(testEntClient(t), integrationDB))
	t.Cleanup(lane.Stop)
	return lane
}

func sourcesLaneNormalCommand(f rejectionBillingFixture, reservation *service.UpstreamRejectionReserveCommand) *service.UsageBillingCommand {
	return &service.UsageBillingCommand{
		RequestID: "lane-normal-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: reservation.Model,
		BalanceCost: .025, APIKeyQuotaCost: .025, APIKeyRateLimitCost: .025,
		RequestPayloadHash: reservation.RequestPayloadHash, UpstreamRejectionReservationID: reservation.RequestID,
	}
}

func TestUsageBillingLaneSourcesNormalHoldReleaseAtomicReplay(t *testing.T) {
	ctx := context.Background()
	f := newRejectionBillingFixture(t, 10)
	lane := newSourcesBillingLane(t)
	var ledger service.UpstreamRejectionBillingRepository = lane
	reservation := f.command()
	reserved, err := ledger.ReserveUpstreamRejection(ctx, reservation)
	require.NoError(t, err)
	require.True(t, reserved.Applied)
	_, err = ledger.MarkUpstreamRejectionSent(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	// The normal terminal event and native monetary effects must share a
	// transaction even after wrapping; a later write failure must undo both.
	command := sourcesLaneNormalCommand(f, reservation)
	removeFailure := failRejectionEvent(t, reservation.RequestID, service.UpstreamRejectionNormal)
	_, err = lane.Apply(ctx, command)
	require.Error(t, err)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionReceiptCount(t, reservation, service.UpstreamRejectionNormal, 0)
	var claims int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, command.RequestID).Scan(&claims))
	require.Zero(t, claims)
	removeFailure()

	for attempt := 0; attempt < 2; attempt++ {
		result, err := lane.Apply(ctx, command)
		require.NoError(t, err)
		require.Equal(t, attempt == 0, result.Applied)
	}
	assertRejectionMoney(t, f, "9.975", "0", "0.025")
	stored, err := ledger.GetUpstreamRejection(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionNormal, stored.State)
	require.Equal(t, command.RequestID, stored.NormalBillingRequestID)
	assertRejectionReceiptCount(t, reservation, service.UpstreamRejectionNormal, 1)
	_, err = ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(reservation))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
	require.Zero(t, lane.Stats().BatchedCommands, "private transaction cannot enter native batch effects")
}

func TestUsageBillingLaneSourcesRejectsRawNegativeBeforeNormalization(t *testing.T) {
	ctx := context.Background()
	f := newRejectionBillingFixture(t, 10)
	lane := newSourcesBillingLane(t)
	var ledger service.UpstreamRejectionBillingRepository = lane
	reservation := f.command()
	_, err := ledger.ReserveUpstreamRejection(ctx, reservation)
	require.NoError(t, err)
	_, err = ledger.MarkUpstreamRejectionSent(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)

	command := sourcesLaneNormalCommand(f, reservation)
	command.BalanceCost = -0.000000001 // Would quantize to zero if the lane normalized it.
	_, err = lane.Apply(ctx, command)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	require.Equal(t, -0.000000001, command.BalanceCost)
	require.Empty(t, command.RequestFingerprint)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	assertRejectionReceiptCount(t, reservation, service.UpstreamRejectionNormal, 0)
	var claims int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, command.RequestID).Scan(&claims))
	require.Zero(t, claims)
	stored, err := ledger.GetUpstreamRejection(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionSent, stored.State)
}

func TestUsageBillingLaneSourcesUnknownRetainsFundsAcrossReconstruction(t *testing.T) {
	ctx := context.Background()
	f := newRejectionBillingFixture(t, 10)
	lane := newSourcesBillingLane(t)
	reservation := f.command()
	_, err := lane.ReserveUpstreamRejection(ctx, reservation)
	require.NoError(t, err)
	_, err = lane.MarkUpstreamRejectionSent(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)
	_, err = lane.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
		RequestID: reservation.RequestID, APIKeyID: reservation.APIKeyID, Outcome: service.UpstreamRejectionUnknown,
	})
	require.NoError(t, err)

	restarted := newSourcesBillingLane(t)
	stored, err := restarted.GetUpstreamRejection(ctx, reservation.RequestID, reservation.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionUnknown, stored.State)
	release := &service.UpstreamRejectionFinalizeCommand{
		RequestID: reservation.RequestID, APIKeyID: reservation.APIKeyID, Outcome: service.UpstreamRejectionReleased,
	}
	_, err = restarted.FinalizeUpstreamRejection(ctx, release)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")
	release.ConfirmedNoCharge = true
	for attempt := 0; attempt < 2; attempt++ {
		result, err := restarted.FinalizeUpstreamRejection(ctx, release)
		require.NoError(t, err)
		require.Equal(t, attempt == 0, result.Applied)
	}
	assertRejectionMoney(t, f, "10", "0", "0")
	assertRejectionReceiptCount(t, reservation, service.UpstreamRejectionReleased, 1)
}

func TestUsageBillingLaneSourcesDeepSeekJournalSettlementAndRecovery(t *testing.T) {
	ctx := context.Background()
	_, checkpoint, supplierID := deepSeekJournalFixture(t)
	lane := newSourcesBillingLane(t)
	var journal service.DeepSeekUsageJournal = lane
	// Use a distinct journal entry so Begin is exercised through the wrapper;
	// the fixture's empty entry is retained until its exact-identity cleanup.
	checkpoint.Command.RequestID = "deepseek-tools:" + uuid.NewString()
	checkpoint.Usage.RequestID = checkpoint.Command.RequestID
	command := checkpoint.Command
	require.NoError(t, journal.BeginDeepSeekUsage(ctx, command.RequestID, command.APIKeyID, command.UserID, command.AccountID, time.Now().Add(time.Minute)))
	require.NoError(t, journal.SaveDeepSeekUsage(ctx, checkpoint))
	waiting, err := journal.SettleDeepSeekUsage(ctx, command.RequestID, command.APIKeyID)
	require.NoError(t, err)
	require.Nil(t, waiting, "an active partial checkpoint must not be settled early")
	checkpoint.Ready = true
	checkpoint.Revision++
	require.NoError(t, journal.SaveDeepSeekUsage(ctx, checkpoint))
	pending, err := journal.ListPendingDeepSeekUsage(ctx, 100)
	require.NoError(t, err)
	require.Contains(t, pending, service.DeepSeekUsageIdentity{RequestID: command.RequestID, APIKeyID: command.APIKeyID})

	// Simulate a restart after persisting confirmed evidence. Recovery has no
	// model client and may safely race/retry without recharging the supplier.
	restarted := newSourcesBillingLane(t)
	var applied atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for attempt := 0; attempt < 4; attempt++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := restarted.SettleDeepSeekUsage(ctx, command.RequestID, command.APIKeyID)
			if err != nil {
				errors <- err
				return
			}
			if result != nil && result.Result.Applied {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), applied.Load())
	var balance, quota, modelCost, supplierCost float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1`, command.UserID).Scan(&balance))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT quota_used FROM api_keys WHERE id=$1`, command.APIKeyID).Scan(&quota))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric FROM accounts WHERE id=$1`, command.AccountID).Scan(&modelCost))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric FROM accounts WHERE id=$1`, supplierID).Scan(&supplierCost))
	require.InDelta(t, .974, balance, 1e-10)
	require.InDelta(t, .026, quota, 1e-10)
	require.InDelta(t, .04, modelCost, 1e-10)
	require.InDelta(t, .01, supplierCost, 1e-10)
	var usageRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE request_id=$1`, command.RequestID).Scan(&usageRows))
	require.Equal(t, 1, usageRows)
	require.Zero(t, restarted.Stats().BatchedCommands)

	emptyID := "deepseek-tools:" + uuid.NewString()
	require.NoError(t, restarted.BeginDeepSeekUsage(ctx, emptyID, command.APIKeyID, command.UserID, command.AccountID, time.Now().Add(-time.Minute)))
	empty, err := restarted.SettleDeepSeekUsage(ctx, emptyID, command.APIKeyID)
	require.NoError(t, err)
	require.Nil(t, empty.Command, "no confirmed evidence must never become a fabricated charge")
	var emptyClaims int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, emptyID).Scan(&emptyClaims))
	require.Zero(t, emptyClaims)
}
