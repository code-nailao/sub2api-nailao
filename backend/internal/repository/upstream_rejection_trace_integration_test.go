//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Supplier-side identifiers in the rejection ledger: stored with an unknown
// outcome, kept through the deadline release, and never a reason to charge.
// The identifiers are fixtures; no supplier, bill or customer is real.

const (
	traceFixtureClientID  = "5b0d7c1e-2f3a-4b6c-8d9e-0a1b2c3d4e5f"
	traceFixtureRequestID = "req-fixture-0001"
	traceFixtureRay       = "8f1c2d3e4a5b6c7d-FIX"
)

func rejectionEvidenceJSON(t *testing.T, query string, args ...any) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, integrationDB.QueryRow(query, args...).Scan(&raw))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded
}

func rejectionRowEvidence(t *testing.T, requestID string) map[string]any {
	t.Helper()
	return rejectionEvidenceJSON(t, `SELECT evidence FROM upstream_rejection_reservations WHERE request_id=$1`, requestID)
}

func rejectionEventEvidence(t *testing.T, requestID, kind string) map[string]any {
	t.Helper()
	return rejectionEvidenceJSON(t, `SELECT evidence FROM upstream_rejection_billing_events WHERE request_id=$1 AND kind=$2`, requestID, kind)
}

func TestUpstreamRejectionObservationSurvivesTheDeadlineRelease(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	c := f.command()
	reserveAndSendRejection(t, f, c)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	observed := service.UpstreamRejectionBillingEvidence{OriginalStatus: 524, UpstreamRequestID: traceFixtureClientID,
		XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}
	unknown, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown, Evidence: observed})
	require.NoError(t, err)
	require.True(t, unknown.Applied)
	want := map[string]any{"original_status": float64(524), "upstream_request_id": traceFixtureClientID,
		"x_client_request_id": traceFixtureClientID, "x_request_id": traceFixtureRequestID, "cf_ray": traceFixtureRay}
	require.Equal(t, want, rejectionRowEvidence(t, c.RequestID))
	require.Equal(t, want, rejectionEventEvidence(t, c.RequestID, service.UpstreamRejectionUnknown))
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	// A later unknown with other identifiers keeps the first durable evidence
	// and does not move the deadline.
	_, before := rejectionDeadlineState(t, c.RequestID)
	again, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown,
		Evidence: service.UpstreamRejectionBillingEvidence{OriginalStatus: 502, CFRay: "another-ray"}})
	require.NoError(t, err)
	require.False(t, again.Applied)
	require.Equal(t, want, rejectionRowEvidence(t, c.RequestID))
	_, after := rejectionDeadlineState(t, c.RequestID)
	require.Equal(t, before, after)
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionUnknown, 1)

	// The supplier's bill line finds the hold while it is still pending.
	var found string
	require.NoError(t, integrationDB.QueryRow(`SELECT request_id FROM upstream_rejection_reservations WHERE evidence->>'x_client_request_id'=$1 AND user_id=$2`, traceFixtureClientID, f.user.ID).Scan(&found))
	require.Equal(t, c.RequestID, found)

	// The deadline release gives the money back exactly and keeps the observation.
	moveRejectionDeadlineIntoPast(t, c.RequestID)
	waiver := service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID)
	released, err := f.ledger.FinalizeUpstreamRejection(ctx, waiver)
	require.NoError(t, err)
	require.True(t, released.Applied)
	assertRejectionMoney(t, f, "10", "0", "0")
	want["release_reason"] = service.UpstreamRejectionReleaseDeadlinePassed
	want["released_from"] = service.UpstreamRejectionUnknown
	require.Equal(t, want, rejectionRowEvidence(t, c.RequestID))
	require.Equal(t, want, rejectionEventEvidence(t, c.RequestID, service.UpstreamRejectionReleased))
	require.True(t, released.Reservation.ReleasedAtDeadline())
	require.False(t, released.Reservation.ReleasedAtDeadlineWhileSent())
	require.Equal(t, traceFixtureRay, released.Reservation.Evidence.CFRay)
	require.Equal(t, 524, released.Reservation.Evidence.OriginalStatus)

	// Replaying the release is idempotent and rewrites nothing.
	replay, err := f.ledger.FinalizeUpstreamRejection(ctx, waiver)
	require.NoError(t, err)
	require.False(t, replay.Applied)
	require.Equal(t, want, rejectionRowEvidence(t, c.RequestID))
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
	assertRejectionMoney(t, f, "10", "0", "0")

	// A late rejection carrying the same identifiers is recorded, not charged.
	late := rejectedBillingCommand(c)
	late.Evidence.XClientRequestID, late.Evidence.CFRay = traceFixtureClientID, traceFixtureRay
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, late)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionWaived)
	result, _ := rejectionLateResult(t, c.RequestID)
	require.Equal(t, false, result["charged"])
	lateEvidence := result["evidence"].(map[string]any)
	require.Equal(t, traceFixtureClientID, lateEvidence["x_client_request_id"])
	require.Equal(t, traceFixtureRay, lateEvidence["cf_ray"])
	assertRejectionMoney(t, f, "10", "0", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 0)
}

func TestUpstreamRejectionDeadlineReleaseWithoutObservationAddsNothing(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()

	// Released while still sent: the process never saw a response.
	sent := f.command()
	reserveAndSendRejection(t, f, sent)
	moveRejectionDeadlineIntoPast(t, sent.RequestID)
	released, err := f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(sent.RequestID, sent.APIKeyID))
	require.NoError(t, err)
	require.True(t, released.Applied)
	require.Equal(t, map[string]any{"release_reason": service.UpstreamRejectionReleaseDeadlinePassed, "released_from": service.UpstreamRejectionSent},
		rejectionRowEvidence(t, sent.RequestID))
	require.True(t, released.Reservation.ReleasedAtDeadlineWhileSent())

	// Unknown without a response (a transport failure): the earlier evidence shape.
	blind := f.command()
	reserveAndSendRejection(t, f, blind)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: blind.RequestID, APIKeyID: blind.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	require.Empty(t, rejectionRowEvidence(t, blind.RequestID))
	moveRejectionDeadlineIntoPast(t, blind.RequestID)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(blind.RequestID, blind.APIKeyID))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"release_reason": service.UpstreamRejectionReleaseDeadlinePassed, "released_from": service.UpstreamRejectionUnknown},
		rejectionRowEvidence(t, blind.RequestID))
	assertRejectionMoney(t, f, "10", "0", "0")
}

func TestUpstreamRejectionIdentifiersNeverChangeAnOutcome(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()

	// An identifier the storage contract refuses is rejected before any
	// transaction: the hold, its state and its deadline are untouched.
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, deadline := rejectionDeadlineState(t, c.RequestID)
	for _, bad := range []service.UpstreamRejectionBillingEvidence{
		{XClientRequestID: "a\x00b"},
		{XRequestID: "标识"},
		{CFRay: strings.Repeat("r", 129)},
	} {
		_, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
			RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown, Evidence: bad})
		require.ErrorIs(t, err, service.ErrUpstreamRejectionInvalid)
	}
	stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, service.UpstreamRejectionSent, stored.State)
	_, unchanged := rejectionDeadlineState(t, c.RequestID)
	require.Equal(t, deadline, unchanged)
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	// A charge keeps its identifiers and is captured exactly once.
	charged := rejectedBillingCommand(c)
	charged.Evidence.XClientRequestID, charged.Evidence.XRequestID, charged.Evidence.CFRay = traceFixtureClientID, traceFixtureRequestID, traceFixtureRay
	first, err := f.ledger.FinalizeUpstreamRejection(ctx, charged)
	require.NoError(t, err)
	require.True(t, first.Applied)
	second, err := f.ledger.FinalizeUpstreamRejection(ctx, charged)
	require.NoError(t, err)
	require.False(t, second.Applied)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionRejected, 1)
	evidence := rejectionEventEvidence(t, c.RequestID, service.UpstreamRejectionRejected)
	require.Equal(t, traceFixtureClientID, evidence["x_client_request_id"])
	require.Equal(t, traceFixtureRequestID, evidence["x_request_id"])
	require.Equal(t, traceFixtureRay, evidence["cf_ray"])

	// The same rejection with a different identifier is a conflict, not a second charge.
	other := *charged
	other.Evidence.XClientRequestID = "another-supplier-id"
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &other)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionConflict)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")

	// A free release confirmed by the upstream keeps status and identifiers.
	free := f.command()
	reserveAndSendRejection(t, f, free)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
		RequestID: free.RequestID, APIKeyID: free.APIKeyID, Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true,
		Evidence: service.UpstreamRejectionBillingEvidence{OriginalStatus: 429, XClientRequestID: traceFixtureClientID}})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"original_status": float64(429), "x_client_request_id": traceFixtureClientID},
		rejectionEventEvidence(t, free.RequestID, service.UpstreamRejectionReleased))
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
	// Replaying that release is idempotent; with another identifier it is a conflict.
	freeCmd := &service.UpstreamRejectionFinalizeCommand{
		RequestID: free.RequestID, APIKeyID: free.APIKeyID, Outcome: service.UpstreamRejectionReleased, ConfirmedNoCharge: true,
		Evidence: service.UpstreamRejectionBillingEvidence{OriginalStatus: 429, XClientRequestID: traceFixtureClientID}}
	replay, err := f.ledger.FinalizeUpstreamRejection(ctx, freeCmd)
	require.NoError(t, err)
	require.False(t, replay.Applied)
	freeCmd.Evidence.XClientRequestID = "another-supplier-id"
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, freeCmd)
	require.ErrorIs(t, err, service.ErrUpstreamRejectionConflict)
	assertRejectionReceiptCount(t, free, service.UpstreamRejectionReleased, 1)
	assertRejectionMoney(t, f, "9.9", "0", "0.1")
}

// Every value the storage contract admits is written to JSONB and read back
// unchanged: the full printable ASCII range in the fixed identifiers, and a
// non-ASCII request id in the account-declared one.
func TestUpstreamRejectionIdentifiersAtTheLimitsOfTheStorageContractAreStored(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	var printable strings.Builder
	for b := byte(0x20); b <= 0x7e; b++ {
		printable.WriteByte(b)
	}
	all := strings.TrimSpace(printable.String()) // quotes, backslash, braces, percent, angle brackets
	require.Len(t, all, 94)
	widest := strings.Repeat("r", 128)
	observed := service.UpstreamRejectionBillingEvidence{OriginalStatus: 502, UpstreamRequestID: "标识-" + all,
		XClientRequestID: all, XRequestID: widest, CFRay: `"};DROP--\`}
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{
		RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown, Evidence: observed})
	require.NoError(t, err)
	stored, err := f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, observed, stored.Evidence)

	moveRejectionDeadlineIntoPast(t, c.RequestID)
	released, err := f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
	require.NoError(t, err)
	require.True(t, released.Applied)
	want := observed
	want.ReleaseReason, want.ReleasedFrom = service.UpstreamRejectionReleaseDeadlinePassed, service.UpstreamRejectionUnknown
	stored, err = f.ledger.GetUpstreamRejection(ctx, c.RequestID, c.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, want, stored.Evidence)
	assertRejectionMoney(t, f, "10", "0", "0")
}
