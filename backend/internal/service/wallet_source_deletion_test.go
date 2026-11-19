package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func deletionSourceFixture(t *testing.T) PortalWalletSource {
	t.Helper()
	var source PortalWalletSource
	require.NoError(t, json.Unmarshal([]byte(`{"id":"source1","name":"New wallet","origin":"https://new.example","account_id":42,"revision":3,"status":"ok","last_balance":{"account_id":42,"remaining":"80","unit":"USD"}}`), &source))
	source.LastBalance.CheckedAt = time.Now().UTC()
	return source
}

func syncDeletionSource(t *testing.T, m *WalletManager, sources ...PortalWalletSource) {
	t.Helper()
	require.NoError(t, m.SyncPortal(context.Background(), PortalWalletSync{GeneratedAt: time.Now().UTC(), Sources: sources}))
}

func TestWalletSourceDeletionReservationSurvivesSyncAndRejectsMembership(t *testing.T) {
	m, repo, _, _ := managedWalletFixture(t)
	ctx := context.Background()
	source := deletionSourceFixture(t)
	syncDeletionSource(t, m, source)
	for _, revision := range []int64{0, 2, 4} {
		_, err := m.ReserveSourceDeletion(ctx, source.ID, revision)
		require.Error(t, err)
	}
	reservation, err := m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.NoError(t, err)
	require.Equal(t, WalletSourceDeletion{SourceID: source.ID, SourceRevision: source.Revision, Reserved: true}, reservation)
	// An explicit retry is idempotent, including after a local unknown outcome.
	retry, err := m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.NoError(t, err)
	require.Equal(t, reservation, retry)

	for _, revision := range []int64{3, 2} {
		incoming := source
		incoming.Revision = revision
		syncDeletionSource(t, m, incoming)
		state, readErr := repo.Read(ctx)
		require.NoError(t, readErr)
		require.Equal(t, int64(3), state.Sources[source.ID].Revision)
		require.Equal(t, int64(3), state.Sources[source.ID].DeletionRevision)
		require.ErrorIs(t, m.BindSource(ctx, 1, source.ID, 3, 0, 9), ErrWalletSourceDeleting)
		_, saveErr := m.Save(ctx, 1, 9, WalletSettingsInput{Name: "New wallet", Provider: "new", SourceID: source.ID, Members: []int64{1}})
		require.ErrorIs(t, saveErr, ErrWalletSourceDeleting)
	}
	// A successful local edit advances the CAS revision and safely reopens use.
	source.Revision++
	source.DeletionRevision = 999 // The bridge never controls server-owned state.
	syncDeletionSource(t, m, source)
	state, err := repo.Read(ctx)
	require.NoError(t, err)
	require.Zero(t, state.Sources[source.ID].DeletionRevision)
	require.NoError(t, m.BindSource(ctx, 1, source.ID, source.Revision, 0, 9))
	_, err = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.ErrorIs(t, err, ErrWalletSourceLinked)
}

func TestWalletSourceDeletionChecksAllCanonicalWalletsAndIgnoresHistoricalBindings(t *testing.T) {
	m, repo, _, _ := managedWalletFixture(t)
	ctx := context.Background()
	source := deletionSourceFixture(t)
	var input PortalWalletSync
	require.NoError(t, json.Unmarshal([]byte(`{"bindings":[{"account_id":1,"source_id":"source1","revision":7}]}`), &input))
	input.Sources = []PortalWalletSource{source}
	input.GeneratedAt = time.Now().UTC()
	require.NoError(t, repo.Change(ctx, func(state *WalletRegistry) error { state.PortalImported = false; return nil }))
	require.NoError(t, m.SyncPortal(ctx, input))
	_, err := m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.ErrorIs(t, err, ErrWalletSourceLinked)
	require.NoError(t, m.BindSource(ctx, 1, "", 0, 7, 9))
	// The obsolete Portal file still points to the source; importing it twice
	// would incorrectly prohibit deleting an explicitly unlinked source.
	input.GeneratedAt = time.Now().UTC()
	require.NoError(t, m.SyncPortal(ctx, input))
	_, err = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.NoError(t, err)
	// Any profile using this source must protect it, not only new:source1.
	require.NoError(t, repo.Change(ctx, func(state *WalletRegistry) error {
		state.Profiles["another-wallet"] = ManagedWallet{ID: "another-wallet", Provider: "new", SourceID: source.ID}
		state.Members[2] = WalletMember{WalletID: "another-wallet", Revision: 1}
		return nil
	}))
	_, err = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.ErrorIs(t, err, ErrWalletSourceLinked)
}

func TestWalletSourceDeletionAndConcurrentMembershipHaveOneWinner(t *testing.T) {
	for _, useSettings := range []bool{false, true} {
		t.Run(map[bool]string{false: "portal_binding", true: "native_settings"}[useSettings], func(t *testing.T) {
			m, repo, _, _ := managedWalletFixture(t)
			ctx := context.Background()
			source := deletionSourceFixture(t)
			for attempt := 0; attempt < 50; attempt++ {
				require.NoError(t, repo.Change(ctx, func(state *WalletRegistry) error {
					state.Members = map[int64]WalletMember{}
					state.Profiles = map[string]ManagedWallet{}
					state.Sources = map[string]PortalWalletSource{source.ID: source}
					return nil
				}))
				start := make(chan struct{})
				var wg sync.WaitGroup
				var bindErr, deleteErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					if useSettings {
						_, bindErr = m.Save(ctx, 1, 9, WalletSettingsInput{Name: "New wallet", Provider: "new", SourceID: source.ID, Members: []int64{1}})
					} else {
						bindErr = m.BindSource(ctx, 1, source.ID, source.Revision, 0, 9)
					}
				}()
				go func() {
					defer wg.Done()
					<-start
					_, deleteErr = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
				}()
				close(start)
				wg.Wait()
				state, err := repo.Read(ctx)
				require.NoError(t, err)
				if deleteErr == nil {
					require.ErrorIs(t, bindErr, ErrWalletSourceDeleting)
					require.Empty(t, state.Members[1].WalletID)
					require.Equal(t, source.Revision, state.Sources[source.ID].DeletionRevision)
				} else {
					require.True(t, errors.Is(deleteErr, ErrWalletSourceLinked), "%v", deleteErr)
					require.NoError(t, bindErr)
					require.Equal(t, "new:"+source.ID, state.Members[1].WalletID)
					require.Zero(t, state.Sources[source.ID].DeletionRevision)
				}
			}
		})
	}
}

func TestWalletSourceDeletionDisappearanceAndUntrustedReservation(t *testing.T) {
	m, repo, _, _ := managedWalletFixture(t)
	ctx := context.Background()
	source := deletionSourceFixture(t)
	source.DeletionRevision = source.Revision
	syncDeletionSource(t, m, source)
	state, err := repo.Read(ctx)
	require.NoError(t, err)
	require.Zero(t, state.Sources[source.ID].DeletionRevision)
	_, err = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.NoError(t, err)
	syncDeletionSource(t, m)
	state, err = repo.Read(ctx)
	require.NoError(t, err)
	require.NotContains(t, state.Sources, source.ID)
	require.ErrorIs(t, m.BindSource(ctx, 1, source.ID, source.Revision, 0, 9), ErrWalletRegistryConflict)
	_, err = m.ReserveSourceDeletion(ctx, source.ID, source.Revision)
	require.ErrorIs(t, err, ErrWalletRegistryConflict)
}
