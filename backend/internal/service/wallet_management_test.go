package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type walletRegistryFixture struct {
	mu    sync.Mutex
	state WalletRegistry
}

func (r *walletRegistryFixture) Read(context.Context) (WalletRegistry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(r.state)
	var copy WalletRegistry
	_ = json.Unmarshal(raw, &copy)
	copy.Initialize()
	return copy, nil
}
func (r *walletRegistryFixture) Change(ctx context.Context, f func(*WalletRegistry) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(r.state)
	var copy WalletRegistry
	_ = json.Unmarshal(raw, &copy)
	copy.Initialize()
	if err := f(&copy); err != nil {
		return err
	}
	r.state = copy
	return nil
}

type walletAccountsFixture struct {
	AccountRepository
	rows map[int64]*Account
}

func (r *walletAccountsFixture) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	rows := []*Account{}
	for _, id := range ids {
		if a := r.rows[id]; a != nil {
			rows = append(rows, a)
		}
	}
	return rows, nil
}
func (r *walletAccountsFixture) List(context.Context, pagination.PaginationParams) ([]Account, *pagination.PaginationResult, error) {
	rows := []Account{}
	for _, a := range r.rows {
		rows = append(rows, *a)
	}
	return rows, &pagination.PaginationResult{Pages: 1}, nil
}

type walletMailFixture struct {
	calls int
	err   error
}

func (m *walletMailFixture) SendEmail(context.Context, string, string, string) error {
	m.calls++
	return m.err
}

func managedWalletFixture(t *testing.T) (*WalletManager, *walletRegistryFixture, *walletSnapshotFixture, *walletMailFixture) {
	t.Helper()
	reader, a := walletFixture(nil)
	t.Cleanup(reader.Stop)
	b := *a
	b.ID = 2
	b.Name = "Second channel"
	b.Credentials = map[string]any{"api_key": "different-key", "base_url": "https://same-supplier.example"}
	a.Name = "First channel"
	accounts := &walletAccountsFixture{rows: map[int64]*Account{1: a, 2: &b}}
	now := time.Now().UTC()
	snapshot := &walletSnapshotFixture{row: &AccountWalletSnapshot{AccountID: 1, Identity: AccountWalletIdentity(a), Value: AccountWallet{Status: "ready", Remaining: "50", Unit: "USD", CheckedAt: &now, LastAttemptAt: &now, AutoRefresh: true}}}
	state := WalletRegistry{Version: 1, PortalImported: true, PortalSeenAt: now}
	state.Initialize()
	repo := &walletRegistryFixture{state: state}
	mail := &walletMailFixture{}
	manager := &WalletManager{Repo: repo, Reader: reader, Accounts: accounts, Mail: mail, IdentitySecret: "01234567890123456789012345678901"}
	reader.store, reader.accounts, reader.Manager = snapshot, accounts, manager
	return manager, repo, snapshot, mail
}

func TestWalletSharedMembershipRequiresConfirmationAndKeepsIdentities(t *testing.T) {
	m, repo, _, _ := managedWalletFixture(t)
	ctx := context.Background()
	input := WalletSettingsInput{Name: "Shared supplier", Provider: "sub", RepresentativeID: 1, Members: []int64{1, 2}, Alert: WalletAlertRule{Recipients: []string{}}}
	_, err := m.Save(ctx, 1, 9, input)
	require.Error(t, err)
	input.ConfirmShared = true
	wallet, err := m.Save(ctx, 1, 9, input)
	require.NoError(t, err)
	accounts, ok := m.Accounts.(*walletAccountsFixture)
	require.True(t, ok)
	a := accounts.rows[2]
	value := m.Reader.Read(ctx, a, false)
	require.Equal(t, "50", value.Remaining)
	require.Equal(t, 2, value.SharedCount)
	require.Equal(t, wallet.ID, value.WalletID)
	require.False(t, m.IsQueryChannel(ctx, a))
	require.True(t, m.IsQueryChannel(ctx, accounts.rows[1]))
	a.Credentials = map[string]any{"api_key": "rotated", "base_url": "https://same-supplier.example"}
	value = m.Reader.Read(ctx, a, false)
	require.Equal(t, "rebind_required", value.Status)
	require.Empty(t, value.Remaining)
	state, _ := repo.Read(ctx)
	require.Len(t, state.Profiles, 1)
	require.False(t, state.Profiles[wallet.ID].Alert.Enabled)
	input.ID, input.Revision = wallet.ID, 0
	_, err = m.Save(ctx, 1, 9, input)
	require.ErrorIs(t, err, ErrWalletRegistryConflict)
}

func TestWalletAlertsPersistOneEpisodeAndRequireTwoSuccessesToRecover(t *testing.T) {
	m, repo, snap, mail := managedWalletFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	wallet, err := m.Save(ctx, 1, 9, WalletSettingsInput{Name: "Shared supplier", Provider: "sub", RepresentativeID: 1, Members: []int64{1}, Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}, NotifyRecovery: true}})
	require.NoError(t, err)
	// Enabling does not alert from the sample which existed before saving.
	m.Tick(ctx)
	require.Zero(t, mail.calls)
	newTime := now.Add(time.Millisecond)
	snap.row.Value.CheckedAt = &newTime
	snap.row.Value.LastAttemptAt = &newTime
	m.Tick(ctx)
	m.Tick(ctx)
	require.Equal(t, 1, mail.calls)
	state, _ := repo.Read(ctx)
	require.Len(t, state.Deliveries, 1)
	require.True(t, state.Profiles[wallet.ID].Alarm.Low)
	observe := func(amount string, success bool, minute int) {
		at := now.Add(time.Duration(minute) * time.Minute)
		value := AccountWallet{Status: "ready", Remaining: amount, Unit: "USD", CheckedAt: &at, LastAttemptAt: &at}
		if !success {
			value.Status = "error"
		}
		require.NoError(t, m.observe(ctx, wallet, walletObservation{Value: value, Names: []string{"First channel"}, Valid: true}, []string{"owner@example.invalid"}, at))
	}
	observe("150", true, 1)
	state, _ = repo.Read(ctx)
	require.True(t, state.Profiles[wallet.ID].Alarm.Low)
	observe("150", false, 2)
	observe("150", true, 3)
	state, _ = repo.Read(ctx)
	require.True(t, state.Profiles[wallet.ID].Alarm.Low)
	observe("150", true, 4)
	state, _ = repo.Read(ctx)
	require.False(t, state.Profiles[wallet.ID].Alarm.Low)
	require.Len(t, state.Deliveries, 2)
	observe("0", true, 5)
	state, _ = repo.Read(ctx)
	require.True(t, state.Profiles[wallet.ID].Alarm.Low)
	require.Len(t, state.Deliveries, 3)
}

func TestWalletMailUnknownOutcomeDoesNotRetry(t *testing.T) {
	m, repo, snap, mail := managedWalletFixture(t)
	ctx := context.Background()
	_, err := m.Save(ctx, 1, 9, WalletSettingsInput{Name: "<script>bad</script>", Provider: "sub", RepresentativeID: 1, Members: []int64{1}, Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}}})
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Millisecond)
	snap.row.Value.CheckedAt = &at
	snap.row.Value.LastAttemptAt = &at
	mail.err = errors.New("close writer: connection lost")
	m.Tick(ctx)
	m.Tick(ctx)
	require.Equal(t, 1, mail.calls)
	state, _ := repo.Read(ctx)
	for _, d := range state.Deliveries {
		require.Equal(t, "uncertain", d.Status)
		require.NotContains(t, d.HTML, "<script>")
	}
}

func TestWalletBridgeImportsExistingNewLinksOnlyOnce(t *testing.T) {
	m, repo, _, _ := managedWalletFixture(t)
	repo.state.PortalImported = false
	accounts, ok := m.Accounts.(*walletAccountsFixture)
	require.True(t, ok)
	a := accounts.rows[1]
	var input PortalWalletSync
	require.NoError(t, json.Unmarshal([]byte(`{"sources":[{"id":"source1","name":"Existing","origin":"https://new.example","account_id":42,"revision":3,"status":"ok","last_balance":{"account_id":42,"remaining":"80","unit":"USD","checked_at":"2026-10-25T00:00:00Z"}}],"bindings":[{"account_id":1,"source_id":"source1","revision":7}]}`), &input))
	input.GeneratedAt = time.Now().UTC()
	input.Bindings[0].Marker = SourcesBalanceMarker(a, m.IdentitySecret)
	require.NoError(t, m.SyncPortal(context.Background(), input))
	state, _ := repo.Read(context.Background())
	require.Equal(t, "new:source1", state.Members[1].WalletID)
	require.False(t, state.Profiles["new:source1"].Alert.Enabled)
	require.NoError(t, m.BindSource(context.Background(), 1, "", 0, 7, 9))
	input.GeneratedAt = input.GeneratedAt.Add(time.Millisecond)
	require.NoError(t, m.SyncPortal(context.Background(), input))
	state, _ = repo.Read(context.Background())
	require.Empty(t, state.Members[1].WalletID, "legacy file must not resurrect an unlinked membership")
}
