package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWalletSharedAlertCanBeDisabledDuringOutageWithoutRebinding(t *testing.T) {
	m, repo, snapshot, _ := managedWalletFixture(t)
	ctx := context.Background()
	input := WalletSettingsInput{Name: "shared", Provider: "sub", RepresentativeID: 1, Members: []int64{1, 2}, ConfirmShared: true,
		Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}}}
	wallet, err := m.Save(ctx, 1, 9, input)
	require.NoError(t, err)
	before, err := repo.Read(ctx)
	require.NoError(t, err)
	accounts, ok := m.Accounts.(*walletAccountsFixture)
	require.True(t, ok)
	accounts.rows[2].Credentials = map[string]any{"api_key": "rotated", "base_url": "https://same-supplier.example"}
	snapshot.row.Value.Status, snapshot.row.Value.Error, snapshot.row.Value.Stale = "error", "upstream_timeout", true
	input.ID, input.Revision, input.ConfirmShared, input.Alert.Enabled = wallet.ID, wallet.Revision, false, false
	_, err = m.Save(ctx, 1, 9, input)
	require.NoError(t, err)
	after, err := repo.Read(ctx)
	require.NoError(t, err)
	require.False(t, after.Profiles[wallet.ID].Alert.Enabled)
	require.Equal(t, before.Members[2], after.Members[2], "disabling a rule must not accept a changed credential")
	require.Equal(t, "rebind_required", m.Reader.Read(ctx, accounts.rows[2], false).Status)
}

func TestWalletSingletonRebindRequiresExplicitIntentAndSuccessfulSample(t *testing.T) {
	m, repo, snapshot, _ := managedWalletFixture(t)
	ctx := context.Background()
	input := WalletSettingsInput{Name: "single", Provider: "sub", RepresentativeID: 1, Members: []int64{1},
		Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}}}
	wallet, err := m.Save(ctx, 1, 9, input)
	require.NoError(t, err)
	accounts, ok := m.Accounts.(*walletAccountsFixture)
	require.True(t, ok)
	a := accounts.rows[1]
	a.Credentials = map[string]any{"api_key": "rotated", "base_url": "https://upstream.example/v1"}
	input.ID, input.Revision = wallet.ID, wallet.Revision
	_, err = m.Save(ctx, 1, 9, input)
	require.ErrorIs(t, err, ErrWalletRebind)
	input.ConfirmShared = true
	_, err = m.Save(ctx, 1, 9, input)
	require.ErrorIs(t, err, ErrWalletNotVerified)
	now := time.Now().UTC()
	snapshot.row.Identity = AccountWalletIdentity(a)
	snapshot.row.Value = AccountWallet{Status: "ready", Remaining: "25", Unit: "USD", CheckedAt: &now}
	_, err = m.Save(ctx, 1, 9, input)
	require.NoError(t, err)
	state, err := repo.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, AccountWalletIdentity(a), state.Members[1].Identity)
	require.Equal(t, wallet.ID, state.Members[1].WalletID)
	require.Equal(t, "25", m.Reader.Read(ctx, a, false).Remaining)
}

func TestWalletNativePreSubmissionSMTPFailuresRetry(t *testing.T) {
	for _, failure := range []string{"tls dial: connection timed out", "starttls: handshake failed", "new smtp client: EOF"} {
		t.Run(failure, func(t *testing.T) {
			m, repo, snapshot, sender := managedWalletFixture(t)
			ctx := context.Background()
			_, err := m.Save(ctx, 1, 9, WalletSettingsInput{Name: "mail", Provider: "sub", RepresentativeID: 1, Members: []int64{1},
				Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}}})
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Millisecond)
			snapshot.row.Value.CheckedAt, snapshot.row.Value.LastAttemptAt = &now, &now
			sender.err = errors.New(failure)
			m.Tick(ctx)
			state, err := repo.Read(ctx)
			require.NoError(t, err)
			require.Len(t, state.Deliveries, 1)
			for _, delivery := range state.Deliveries {
				require.Equal(t, "retry", delivery.Status)
				require.Equal(t, 1, delivery.Attempts)
				require.True(t, delivery.NextAttempt.After(delivery.UpdatedAt))
			}
			m.Tick(ctx)
			require.Equal(t, 1, sender.calls, "retry must obey backoff")
		})
	}
}
