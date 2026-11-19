//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Deadline waiver: a synchronous hold whose outcome stays unknown past its
// durable deadline is released without charge; late results are recorded once.

func moveRejectionDeadlineIntoPast(t *testing.T, requestID string) {
	t.Helper()
	res, err := integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NOW()-INTERVAL '1 second' WHERE request_id=$1`, requestID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

func rejectionDeadlineState(t *testing.T, requestID string) (hasDeadline bool, secondsAfterUpdate float64) {
	t.Helper()
	var seconds *float64
	require.NoError(t, integrationDB.QueryRow(`SELECT EXTRACT(EPOCH FROM release_after-updated_at)::float8 FROM upstream_rejection_reservations WHERE request_id=$1`, requestID).Scan(&seconds))
	if seconds == nil {
		return false, 0
	}
	return true, *seconds
}

func rejectionLateResult(t *testing.T, requestID string) (map[string]any, string) {
	t.Helper()
	var raw []byte
	require.NoError(t, integrationDB.QueryRow(`SELECT late_result FROM upstream_rejection_reservations WHERE request_id=$1`, requestID).Scan(&raw))
	if raw == nil {
		return nil, ""
	}
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded, string(raw)
}

func rejectionDueContains(t *testing.T, f rejectionBillingFixture, requestID string) bool {
	t.Helper()
	source, ok := f.repo.(service.UpstreamRejectionDueSource)
	require.True(t, ok, "production repository must list due holds")
	var after *service.UpstreamRejectionDueCursor
	for {
		page, err := source.ListDueUpstreamRejectionsPage(context.Background(), &service.UpstreamRejectionDueQuery{Limit: 100, After: after})
		require.NoError(t, err)
		for _, item := range page.Items {
			require.Nil(t, item.Task)
			require.Contains(t, []string{service.UpstreamRejectionSent, service.UpstreamRejectionUnknown}, item.State)
			if item.RequestID == requestID {
				return true
			}
		}
		if page.Next == nil {
			return false
		}
		after = page.Next
	}
}

func normalBillForRejection(f rejectionBillingFixture, c *service.UpstreamRejectionReserveCommand) *service.UsageBillingCommand {
	return &service.UsageBillingCommand{
		RequestID: "normal-" + uuid.NewString(), UserID: f.user.ID, APIKeyID: f.key.ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeAPIKey, Model: c.Model,
		InputTokens: 12, OutputTokens: 34,
		BalanceCost: 0.025, APIKeyQuotaCost: 0.025, APIKeyRateLimitCost: 0.025,
		RequestPayloadHash: c.RequestPayloadHash, UpstreamRejectionReservationID: c.RequestID,
		UpstreamRequestID: "supplier-" + uuid.NewString(),
	}
}

func TestUpstreamRejectionDeadlineSetOnSendShortenedOnUnknownClearedOnBind(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	grace := service.UpstreamRejectionReleaseGrace.Seconds()
	lifetime := (service.UpstreamRejectionMaxRequestLifetime + service.UpstreamRejectionReleaseGrace).Seconds()

	c := f.command()
	_, err := f.ledger.ReserveUpstreamRejection(ctx, c)
	require.NoError(t, err)
	has, _ := rejectionDeadlineState(t, c.RequestID)
	require.False(t, has, "an unsent hold is released by the unsent recovery, not the deadline")
	sent, err := f.ledger.MarkUpstreamRejectionSent(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.NotNil(t, sent.Reservation.ReleaseAfter)
	has, seconds := rejectionDeadlineState(t, c.RequestID)
	require.True(t, has)
	require.Equal(t, lifetime, seconds, "sent deadline is max lifetime plus grace on the database clock")
	require.False(t, rejectionDueContains(t, f, c.RequestID))

	unknown := &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown}
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, unknown)
	require.NoError(t, err)
	has, seconds = rejectionDeadlineState(t, c.RequestID)
	require.True(t, has)
	require.Equal(t, grace, seconds, "unknown shortens the deadline to one grace period")

	// An earlier deadline is never extended by a later unknown.
	early := f.command()
	reserveAndSendRejection(t, f, early)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NOW()+INTERVAL '1 minute' WHERE request_id=$1`, early.RequestID)
	require.NoError(t, err)
	var before string
	require.NoError(t, integrationDB.QueryRow(`SELECT release_after::text FROM upstream_rejection_reservations WHERE request_id=$1`, early.RequestID).Scan(&before))
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: early.RequestID, APIKeyID: early.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	var after string
	require.NoError(t, integrationDB.QueryRow(`SELECT release_after::text FROM upstream_rejection_reservations WHERE request_id=$1`, early.RequestID).Scan(&after))
	require.Equal(t, before, after)

	// A sent row without a deadline was already waiting before deadlines
	// existed: becoming unknown never gives it one, so the timer cannot sweep it.
	historical := f.command()
	reserveAndSendRejection(t, f, historical)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NULL WHERE request_id=$1`, historical.RequestID)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: historical.RequestID, APIKeyID: historical.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	has, _ = rejectionDeadlineState(t, historical.RequestID)
	require.False(t, has, "a hold waiting before the change keeps NULL forever")

	// An uncertain send (reserved -> unknown) gets exactly one grace period.
	uncertain := f.command()
	_, err = f.ledger.ReserveUpstreamRejection(ctx, uncertain)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: uncertain.RequestID, APIKeyID: uncertain.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	has, seconds = rejectionDeadlineState(t, uncertain.RequestID)
	require.True(t, has)
	require.Equal(t, grace, seconds)

	// Binding a task clears the deadline from sent and from unknown, and a
	// task-bound unknown stays without one.
	for _, markUnknown := range []bool{false, true} {
		v := f.command()
		reserveAndSendRejection(t, f, v)
		if markUnknown {
			_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: v.RequestID, APIKeyID: v.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
			require.NoError(t, err)
		}
		require.NoError(t, f.ledger.BindUpstreamRejectionTask(ctx, &service.UpstreamRejectionTaskBinding{RequestID: v.RequestID, APIKeyID: v.APIKeyID, TaskID: "task-" + uuid.NewString()}))
		has, _ = rejectionDeadlineState(t, v.RequestID)
		require.False(t, has)
		stored, err := f.ledger.GetUpstreamRejection(ctx, v.RequestID, v.APIKeyID)
		require.NoError(t, err)
		require.Nil(t, stored.ReleaseAfter)
		if !markUnknown {
			_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: v.RequestID, APIKeyID: v.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
			require.NoError(t, err)
			has, _ = rejectionDeadlineState(t, v.RequestID)
			require.False(t, has, "a task-bound hold is settled by its task, never by the timer")
		}
	}
	assertRejectionMoney(t, f, "9.4", "0.6", "0")
}

func TestUpstreamRejectionDeadlineWaiverPreconditions(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()

	reserved := f.command()
	_, err := f.ledger.ReserveUpstreamRejection(ctx, reserved)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(reserved.RequestID, reserved.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "an unsent hold is not a waiver case")

	early := f.command()
	reserveAndSendRejection(t, f, early)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(early.RequestID, early.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "refused before the deadline")
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: early.RequestID, APIKeyID: early.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(early.RequestID, early.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "refused before the shortened deadline")

	task := f.command()
	reserveAndSendRejection(t, f, task)
	require.NoError(t, f.ledger.BindUpstreamRejectionTask(ctx, &service.UpstreamRejectionTaskBinding{RequestID: task.RequestID, APIKeyID: task.APIKeyID, TaskID: "task-" + uuid.NewString()}))
	moveRejectionDeadlineIntoPast(t, task.RequestID)
	require.False(t, rejectionDueContains(t, f, task.RequestID))
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(task.RequestID, task.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "a task-bound hold is never released by the timer")

	historical := f.command()
	reserveAndSendRejection(t, f, historical)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: historical.RequestID, APIKeyID: historical.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NULL,created_at=NOW()-INTERVAL '1 year',updated_at=NOW()-INTERVAL '1 year' WHERE request_id=$1`, historical.RequestID)
	require.NoError(t, err)
	require.False(t, rejectionDueContains(t, f, historical.RequestID))
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(historical.RequestID, historical.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "a hold without a deadline keeps today's behaviour")

	// The reason is server-owned and the waiver carries nothing else.
	forged := rejectedBillingCommand(early)
	forged.Outcome = service.UpstreamRejectionReleased
	forged.Evidence.ReleaseReason = service.UpstreamRejectionReleaseDeadlinePassed
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, forged)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	fromSent := service.NewUpstreamRejectionDeadlineReleaseCommand(early.RequestID, early.APIKeyID)
	fromSent.Evidence.ReleasedFrom = service.UpstreamRejectionSent
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, fromSent)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid, "the released-from state is never chosen by the caller")
	both := service.NewUpstreamRejectionDeadlineReleaseCommand(early.RequestID, early.APIKeyID)
	both.ConfirmedNoCharge = true
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, both)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)

	assertRejectionMoney(t, f, "9.6", "0.4", "0")
	assertRejectionReceiptCount(t, early, service.UpstreamRejectionReleased, 0)
	assertRejectionReceiptCount(t, task, service.UpstreamRejectionReleased, 0)
	assertRejectionReceiptCount(t, historical, service.UpstreamRejectionReleased, 0)
}

func TestUpstreamRejectionDeadlineWaiverRestoresMoneyExactlyAndIsIdempotent(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	// Historical frozen money belongs to other holds and must stay untouched.
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE api_keys SET quota=0.1 WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err = f.ledger.ReserveUpstreamRejection(ctx, f.command())
	require.ErrorIs(t, err, service.ErrAPIKeyQuotaExhausted, "the hold reserves the key budget")
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	moveRejectionDeadlineIntoPast(t, c.RequestID)
	require.True(t, rejectionDueContains(t, f, c.RequestID))

	waiver := service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID)
	first, err := f.ledger.FinalizeUpstreamRejection(ctx, waiver)
	require.NoError(t, err)
	require.True(t, first.Applied)
	require.True(t, first.Reservation.ReleasedAtDeadline())
	require.True(t, decimal.NewFromFloat(first.NewBalance).Equal(decimal.RequireFromString("10")))
	assertRejectionMoney(t, f, "10", "7", "0")
	second, err := f.ledger.FinalizeUpstreamRejection(ctx, waiver)
	require.NoError(t, err)
	require.False(t, second.Applied)
	assertRejectionMoney(t, f, "10", "7", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
	var reason string
	require.NoError(t, integrationDB.QueryRow(`SELECT evidence->>'release_reason' FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind='released'`, c.RequestID).Scan(&reason))
	require.Equal(t, service.UpstreamRejectionReleaseDeadlinePassed, reason)
	var from string
	require.NoError(t, integrationDB.QueryRow(`SELECT evidence->>'released_from' FROM upstream_rejection_reservations WHERE request_id=$1`, c.RequestID).Scan(&from))
	require.Equal(t, service.UpstreamRejectionUnknown, from, "the repository records the state it released from")
	require.False(t, rejectionDueContains(t, f, c.RequestID))
	stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.True(t, stored.ReleasedAtDeadline())

	// A confirmed release of another hold keeps its historical, reason-less evidence.
	confirmed := f.command()
	_, err = integrationDB.Exec(`UPDATE api_keys SET quota=0 WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	reserveAndSendRejection(t, f, confirmed)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: confirmed.RequestID, APIKeyID: confirmed.APIKeyID, Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true})
	require.NoError(t, err)
	var evidence string
	require.NoError(t, integrationDB.QueryRow(`SELECT evidence::text FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind='released'`, confirmed.RequestID).Scan(&evidence))
	require.Equal(t, "{}", evidence)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(confirmed.RequestID, confirmed.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "a hold released for another reason is a lost race")
	assertRejectionMoney(t, f, "10", "7", "0")

	// The key budget is free again: a new admission against the original quota succeeds.
	_, err = integrationDB.Exec(`UPDATE api_keys SET quota=0.1 WHERE id=$1`, f.key.ID)
	require.NoError(t, err)
	_, err = f.ledger.ReserveUpstreamRejection(ctx, f.command())
	require.NoError(t, err)
	assertRejectionMoney(t, f, "9.9", "7.1", "0")
}

func TestUpstreamRejectionLateResultsAfterWaiverAreRecordedOnceWithoutCharge(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	// Released from unknown: the gateway saw these requests end without a
	// usable result, so the site absorbs them.
	waive := func(c *service.UpstreamRejectionReserveCommand) {
		reserveAndSendRejection(t, f, c)
		_, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
		require.NoError(t, err)
		moveRejectionDeadlineIntoPast(t, c.RequestID)
		result, err := f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
		require.NoError(t, err)
		require.True(t, result.Applied)
	}

	// A late normal bill, replayed, then a late rejection: one record, no money.
	c := f.command()
	waive(c)
	bill := normalBillForRejection(f, c)
	for i := 0; i < 2; i++ {
		result, err := f.repo.Apply(ctx, bill)
		require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
		require.Nil(t, result)
	}
	late, raw := rejectionLateResult(t, c.RequestID)
	require.Equal(t, service.UpstreamRejectionNormal, late["kind"])
	require.Equal(t, bill.RequestID, late["billing_request_id"])
	require.EqualValues(t, 0.025, late["amount"])
	require.Equal(t, false, late["charged"])
	require.NotEmpty(t, late["recorded_at"])
	evidence := late["evidence"].(map[string]any)
	require.Equal(t, bill.UpstreamRequestID, evidence["upstream_request_id"])
	require.EqualValues(t, 12, evidence["input_tokens"])
	require.EqualValues(t, 34, evidence["output_tokens"])
	_, err := f.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(c))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
	_, again := rejectionLateResult(t, c.RequestID)
	require.Equal(t, raw, again, "the first late result is kept once")

	// A late rejection, replayed: recorded once, never captured.
	r := f.command()
	waive(r)
	rejection := rejectedBillingCommand(r)
	for i := 0; i < 2; i++ {
		result, err := f.ledger.FinalizeUpstreamRejection(ctx, rejection)
		require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
		require.Nil(t, result)
	}
	late, _ = rejectionLateResult(t, r.RequestID)
	require.Equal(t, service.UpstreamRejectionRejected, late["kind"])
	require.EqualValues(t, 0.1, late["amount"])
	require.Equal(t, false, late["charged"])
	require.Equal(t, rejection.Evidence.UpstreamRequestID, late["evidence"].(map[string]any)["upstream_request_id"])
	stored, err := f.ledger.GetUpstreamRejection(ctx, r.RequestID, r.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionReleased, stored.State)
	require.Nil(t, stored.UsageProjectedAt)

	for _, id := range []*service.UpstreamRejectionReserveCommand{c, r} {
		assertRejectionReceiptCount(t, id, service.UpstreamRejectionReleased, 1)
		assertRejectionReceiptCount(t, id, service.UpstreamRejectionRejected, 0)
		assertRejectionReceiptCount(t, id, service.UpstreamRejectionNormal, 0)
	}
	var claims int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, f.key.ID).Scan(&claims))
	require.Zero(t, claims)
	assertRejectionMoney(t, f, "10", "0", "0")

	// A hold released for another reason keeps today's behaviour: no record.
	other := f.command()
	reserveAndSendRejection(t, f, other)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: other.RequestID, APIKeyID: other.APIKeyID, Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true})
	require.NoError(t, err)
	_, err = f.repo.Apply(ctx, normalBillForRejection(f, other))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(other))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState)
	late, _ = rejectionLateResult(t, other.RequestID)
	require.Nil(t, late)
	assertRejectionMoney(t, f, "10", "0", "0")
}

// raceUntilBothWin runs a race round at least minRounds times and keeps going
// (up to maxRounds) until each side has won at least once, then requires it.
// A race test that only ever observes one interleaving proves nothing.
func raceUntilBothWin(t *testing.T, minRounds, maxRounds int, round func() (firstWon bool)) (first, second int) {
	t.Helper()
	for i := 0; i < maxRounds && (i < minRounds || first == 0 || second == 0); i++ {
		if round() {
			first++
		} else {
			second++
		}
	}
	t.Logf("first side won %d, second side won %d", first, second)
	require.Positive(t, first, "the first side never won the race")
	require.Positive(t, second, "the second side never won the race")
	return first, second
}

func raceTwo(a, b func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a() }()
	go func() { defer wg.Done(); <-start; b() }()
	close(start)
	wg.Wait()
}

func dueRejection(t *testing.T, f rejectionBillingFixture, unknown bool) *service.UpstreamRejectionReserveCommand {
	t.Helper()
	c := f.command()
	reserveAndSendRejection(t, f, c)
	if unknown {
		_, err := f.ledger.FinalizeUpstreamRejection(context.Background(), &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
		require.NoError(t, err)
	}
	moveRejectionDeadlineIntoPast(t, c.RequestID)
	return c
}

func TestUpstreamRejectionDeadlineWaiverRaceWithRejectionHasOneOutcome(t *testing.T) {
	f := newRejectionBillingFixture(t, 1000)
	ctx := context.Background()
	round := 0
	rejected, _ := raceUntilBothWin(t, 40, 400, func() bool {
		round++
		c := dueRejection(t, f, round%2 == 0)
		var waiverErr, rejectErr error
		var waiverResult, rejectResult *service.UpstreamRejectionBillingResult
		raceTwo(func() {
			waiverResult, waiverErr = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
		}, func() {
			rejectResult, rejectErr = f.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(c))
		})
		stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
		require.NoError(t, err)
		late, _ := rejectionLateResult(t, c.RequestID)
		switch stored.State {
		case service.UpstreamRejectionRejected:
			require.NoError(t, rejectErr)
			require.True(t, rejectResult.Applied)
			require.ErrorIs(t, waiverErr, service.ErrUpstreamRejectionState)
			require.Nil(t, late)
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 0)
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
			return true
		case service.UpstreamRejectionReleased:
			require.NoError(t, waiverErr)
			require.True(t, waiverResult.Applied)
			// A late rejection is never charged, whichever state was released.
			require.ErrorIs(t, rejectErr, service.ErrUpstreamRejectionWaived)
			require.Equal(t, service.UpstreamRejectionRejected, late["kind"])
			require.Equal(t, false, late["charged"])
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
			return false
		default:
			t.Fatalf("unexpected race winner state: %s", stored.State)
			return false
		}
	})
	spent := decimal.RequireFromString("0.1").Mul(decimal.NewFromInt(int64(rejected)))
	assertRejectionMoney(t, f, decimal.RequireFromString("1000").Sub(spent).String(), "0", spent.String())
	var claims int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, f.key.ID).Scan(&claims))
	require.Equal(t, rejected, claims)
}

// raceWaiverWithNormalBill races the waiver against an ordinary bill and
// returns whether the bill won, plus the number of ordinary charges (0 or 1).
func raceWaiverWithNormalBill(t *testing.T, f rejectionBillingFixture, unknown bool) (billWon bool, charges int) {
	t.Helper()
	ctx := context.Background()
	c := dueRejection(t, f, unknown)
	bill := normalBillForRejection(f, c)
	var waiverErr, billErr error
	var waiverResult *service.UpstreamRejectionBillingResult
	var billResult *service.UsageBillingApplyResult
	raceTwo(func() {
		waiverResult, waiverErr = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
	}, func() {
		billResult, billErr = f.repo.Apply(ctx, bill)
	})
	stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	late, _ := rejectionLateResult(t, c.RequestID)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, bill.RequestID, bill.APIKeyID).Scan(&charges))
	// A replay never charges twice, whoever won.
	if replay, replayErr := f.repo.Apply(ctx, bill); replayErr == nil {
		require.False(t, replay.Applied)
	} else {
		require.ErrorIs(t, replayErr, service.ErrUpstreamRejectionWaived)
	}
	switch stored.State {
	case service.UpstreamRejectionNormal:
		require.NoError(t, billErr)
		require.True(t, billResult.Applied)
		require.ErrorIs(t, waiverErr, service.ErrUpstreamRejectionState)
		require.Nil(t, late)
		require.Equal(t, 1, charges)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 0)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 1)
		return true, charges
	case service.UpstreamRejectionReleased:
		require.NoError(t, waiverErr)
		require.True(t, waiverResult.Applied)
		require.Equal(t, service.UpstreamRejectionNormal, late["kind"])
		require.Equal(t, bill.RequestID, late["billing_request_id"])
		if unknown {
			require.ErrorIs(t, billErr, service.ErrUpstreamRejectionWaived)
			require.Equal(t, false, late["charged"])
			require.Zero(t, charges, "after a release from unknown the late bill is not charged")
		} else {
			require.NoError(t, billErr)
			require.True(t, billResult.Applied)
			require.Equal(t, true, late["charged"])
			require.Equal(t, 1, charges, "after a release from sent the ordinary bill is still owed once")
		}
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 0)
		return false, charges
	default:
		t.Fatalf("unexpected race winner state: %s", stored.State)
		return false, 0
	}
}

func TestUpstreamRejectionDeadlineWaiverRaceWithNormalBillHasOneOutcome(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		name := "released_from_sent"
		if unknown {
			name = "released_from_unknown"
		}
		t.Run(name, func(t *testing.T) {
			f := newRejectionBillingFixture(t, 1000)
			total := 0
			raceUntilBothWin(t, 40, 400, func() bool {
				won, charges := raceWaiverWithNormalBill(t, f, unknown)
				total += charges
				return won
			})
			spent := decimal.RequireFromString("0.025").Mul(decimal.NewFromInt(int64(total)))
			assertRejectionMoney(t, f, decimal.RequireFromString("1000").Sub(spent).String(), "0", spent.String())
		})
	}
}

func TestUpstreamRejectionDeadlineWaiverRaceWithTaskBindingHasOneOutcome(t *testing.T) {
	f := newRejectionBillingFixture(t, 1000)
	ctx := context.Background()
	released := 0
	raceUntilBothWin(t, 40, 400, func() bool {
		c := dueRejection(t, f, false)
		binding := &service.UpstreamRejectionTaskBinding{RequestID: c.RequestID, APIKeyID: c.APIKeyID, TaskID: "task-" + uuid.NewString()}
		var waiverErr, bindErr error
		var waiverResult *service.UpstreamRejectionBillingResult
		raceTwo(func() {
			waiverResult, waiverErr = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
		}, func() {
			bindErr = f.ledger.BindUpstreamRejectionTask(ctx, binding)
		})
		stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
		require.NoError(t, err)
		if stored.Task != nil {
			require.NoError(t, bindErr)
			require.ErrorIs(t, waiverErr, service.ErrUpstreamRejectionState)
			require.Equal(t, service.UpstreamRejectionSent, stored.State)
			require.Nil(t, stored.ReleaseAfter, "a bound hold is settled by its task, never by the timer")
			assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 0)
			assertRejectionReceiptCount(t, c, "task_bound", 1)
			return true
		}
		released++
		require.ErrorIs(t, bindErr, service.ErrUpstreamRejectionState)
		require.NoError(t, waiverErr)
		require.True(t, waiverResult.Applied)
		require.Equal(t, service.UpstreamRejectionReleased, stored.State)
		assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
		assertRejectionReceiptCount(t, c, "task_bound", 0)
		return false
	})
	var bound int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM upstream_rejection_reservations WHERE user_id=$1 AND state='sent' AND task_id IS NOT NULL`, f.user.ID).Scan(&bound))
	held := decimal.RequireFromString("0.1").Mul(decimal.NewFromInt(int64(bound)))
	assertRejectionMoney(t, f, decimal.RequireFromString("1000").Sub(held).String(), held.String(), "0")
	require.Positive(t, released)
}

func TestUpstreamRejectionDeadlineReleaseFromSentStillChargesLateOrdinaryBillOnce(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	// Historical frozen money belongs to other holds and must stay untouched.
	_, err := integrationDB.Exec(`UPDATE users SET frozen_balance=7 WHERE id=$1`, f.user.ID)
	require.NoError(t, err)
	c := dueRejection(t, f, false)
	result, err := f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.Reservation.ReleasedAtDeadlineWhileSent())
	assertRejectionMoney(t, f, "10", "7", "0")

	bill := normalBillForRejection(f, c)
	for i := 0; i < 2; i++ {
		applied, err := f.repo.Apply(ctx, bill)
		require.NoError(t, err)
		require.Equal(t, i == 0, applied.Applied, "the ordinary bill is charged exactly once")
		assertRejectionMoney(t, f, "9.975", "7", "0.025")
	}
	late, raw := rejectionLateResult(t, c.RequestID)
	require.Equal(t, service.UpstreamRejectionNormal, late["kind"])
	require.Equal(t, true, late["charged"])
	require.EqualValues(t, 0.025, late["amount"])

	// A late rejection after a release from sent is still never charged.
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, rejectedBillingCommand(c))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
	_, again := rejectionLateResult(t, c.RequestID)
	require.Equal(t, raw, again)
	assertRejectionMoney(t, f, "9.975", "7", "0.025")
	stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionReleased, stored.State)
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionNormal, 0)
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)

	// The same after a release from unknown: recorded, not charged.
	u := dueRejection(t, f, true)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(u.RequestID, u.APIKeyID))
	require.NoError(t, err)
	_, err = f.repo.Apply(ctx, normalBillForRejection(f, u))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
	late, _ = rejectionLateResult(t, u.RequestID)
	require.Equal(t, false, late["charged"])
	assertRejectionMoney(t, f, "9.975", "7", "0.025")
}
