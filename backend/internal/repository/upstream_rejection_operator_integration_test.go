//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// What an operator can do to the deadline release: pause it, and hand a
// reviewed historical hold to it.

// The pause switch of the deadline release lives in the settings table. Every
// deployment that never set it depends on a missing key being reported as
// "not found", which the sweep reads as "not paused".
func TestUpstreamRejectionPauseSwitchSettingRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := NewSettingRepository(integrationEntClient)
	key := service.SettingKeyUpstreamRejectionDeadlineReleasePaused
	t.Cleanup(func() { _ = repo.Delete(context.Background(), key) })
	require.NoError(t, repo.Delete(ctx, key))

	_, err := repo.GetValue(ctx, key)
	require.ErrorIs(t, err, service.ErrSettingNotFound)

	for _, value := range []string{"true", "false", "true"} {
		require.NoError(t, repo.Set(ctx, key, value))
		got, err := repo.GetValue(ctx, key)
		require.NoError(t, err)
		require.Equal(t, value, got)
	}
	// The operator's statement is a plain upsert of one row.
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,'false',NOW())
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, key)
	require.NoError(t, err)
	got, err := repo.GetValue(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "false", got)
}

// A hold that was already waiting before deadlines existed has no deadline and
// is never swept. After reviewing it, an operator hands it to the timer by
// giving it one; the release then runs through the same audited path as any
// other deadline release. Nothing else about the row has to be edited by hand.
func TestUpstreamRejectionReviewedHistoricalHoldIsReleasedOnceItHasADeadline(t *testing.T) {
	f := newRejectionBillingFixture(t, 10)
	ctx := context.Background()
	c := f.command()
	reserveAndSendRejection(t, f, c)
	_, err := integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NULL WHERE request_id=$1`, c.RequestID)
	require.NoError(t, err)
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, &service.UpstreamRejectionFinalizeCommand{RequestID: c.RequestID, APIKeyID: c.APIKeyID, Outcome: service.UpstreamRejectionUnknown})
	require.NoError(t, err)
	has, _ := rejectionDeadlineState(t, c.RequestID)
	require.False(t, has, "becoming unknown gives a historical hold no deadline")
	require.False(t, rejectionDueContains(t, f, c.RequestID))
	_, err = f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
	require.ErrorIs(t, err, service.ErrUpstreamRejectionState, "without a deadline the release is refused")
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	// The operator's one statement: a timestamp, no money column.
	res, err := integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NOW()
		WHERE request_id=$1 AND state IN ('sent','unknown') AND task_id IS NULL AND release_after IS NULL`, c.RequestID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.True(t, rejectionDueContains(t, f, c.RequestID))
	assertRejectionMoney(t, f, "9.9", "0.1", "0")

	released, err := f.ledger.FinalizeUpstreamRejection(ctx, service.NewUpstreamRejectionDeadlineReleaseCommand(c.RequestID, c.APIKeyID))
	require.NoError(t, err)
	require.True(t, released.Applied)
	assertRejectionMoney(t, f, "10", "0", "0")
	assertRejectionReceiptCount(t, c, service.UpstreamRejectionReleased, 1)
	require.Equal(t, map[string]any{"release_reason": service.UpstreamRejectionReleaseDeadlinePassed, "released_from": service.UpstreamRejectionUnknown},
		rejectionEventEvidence(t, c.RequestID, service.UpstreamRejectionReleased))

	// The same statement cannot touch a hold that is already settled.
	res, err = integrationDB.Exec(`UPDATE upstream_rejection_reservations SET release_after=NOW()
		WHERE request_id=$1 AND state IN ('sent','unknown') AND task_id IS NULL AND release_after IS NULL`, c.RequestID)
	require.NoError(t, err)
	n, err = res.RowsAffected()
	require.NoError(t, err)
	require.Zero(t, n)
}
