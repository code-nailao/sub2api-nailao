//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWalletRegistryAtomicEditsSuppressDuplicateSchedules(t *testing.T) {
	ctx := context.Background()
	repo := &walletRegistryRepository{db: integrationDB}
	var original []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1`).Scan(&original))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `UPDATE sources_wallet_management SET value=$1::jsonb WHERE id=1`, string(original))
	})
	accounts := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	first := &service.Account{Name: "wallet-shared-first", Platform: "openai", Type: "apikey", Status: "active", Concurrency: 1, Priority: 50, Credentials: map[string]any{"api_key": "fixture-first", "base_url": "https://wallet.example"}}
	second := &service.Account{Name: "wallet-shared-second", Platform: "openai", Type: "apikey", Status: "active", Concurrency: 1, Priority: 50, Credentials: map[string]any{"api_key": "fixture-second", "base_url": "https://wallet.example"}}
	for _, account := range []*service.Account{first, second} {
		require.NoError(t, accounts.Create(ctx, account))
		persisted, err := accounts.GetByID(ctx, account.ID)
		require.NoError(t, err)
		*account = *persisted // Match the identity used by real database readers.
		t.Cleanup(func() { _ = testEntClient(t).Account.DeleteOneID(account.ID).Exec(context.Background()) })
	}
	snapshots := &accountWalletSnapshotRepository{sql: integrationDB}
	now, next := time.Now().UTC(), time.Now().UTC().Add(5*time.Minute)
	for _, account := range []*service.Account{first, second} {
		identity := service.AccountWalletIdentity(account)
		ok, err := snapshots.Claim(ctx, account.ID, identity, "fixture-lease", false)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = snapshots.Complete(ctx, account.ID, identity, "fixture-lease", service.AccountWallet{Status: "ready", Remaining: "20", Unit: "USD", CheckedAt: &now, NextCheckAt: &next, AutoRefresh: true})
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.NoError(t, repo.Change(ctx, func(s *service.WalletRegistry) error {
		s.Profiles["sub:fixture"] = service.ManagedWallet{ID: "sub:fixture", Revision: 1, Provider: "sub", RepresentativeID: second.ID}
		for _, account := range []*service.Account{first, second} {
			s.Members[account.ID] = service.WalletMember{WalletID: "sub:fixture", Identity: service.AccountWalletIdentity(account), Revision: 1}
		}
		return nil
	}))
	var firstDue, secondDue bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT next_check_at IS NOT NULL FROM sources_account_wallet_snapshots WHERE account_id=$1`, first.ID).Scan(&firstDue))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT next_check_at IS NOT NULL FROM sources_account_wallet_snapshots WHERE account_id=$1`, second.ID).Scan(&secondDue))
	require.False(t, firstDue)
	require.True(t, secondDue)
	require.NoError(t, repo.Change(ctx, func(s *service.WalletRegistry) error {
		wallet := s.Profiles["sub:fixture"]
		wallet.RepresentativeID = first.ID
		s.Profiles[wallet.ID] = wallet
		return nil
	}))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT next_check_at IS NOT NULL FROM sources_account_wallet_snapshots WHERE account_id=$1`, first.ID).Scan(&firstDue))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT next_check_at IS NOT NULL FROM sources_account_wallet_snapshots WHERE account_id=$1`, second.ID).Scan(&secondDue))
	require.True(t, firstDue, "verified replacement must resume automatic refresh")
	require.False(t, secondDue, "old query channel must stop its independent schedule")
	require.Error(t, repo.Change(ctx, func(s *service.WalletRegistry) error {
		delete(s.Profiles, "sub:fixture")
		return errors.New("rollback fixture")
	}))
	state, err := repo.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, state.Profiles, "sub:fixture")
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			failures <- repo.Change(ctx, func(s *service.WalletRegistry) error {
				wallet := s.Profiles["sub:fixture"]
				wallet.Revision++
				s.Profiles[wallet.ID] = wallet
				return nil
			})
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	state, err = repo.Read(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, state.Profiles["sub:fixture"].Revision)
	require.NoError(t, repo.Change(ctx, func(s *service.WalletRegistry) error {
		s.Members[first.ID] = service.WalletMember{Revision: 2}
		return nil
	}))
	row, err := snapshots.Get(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, "unlinked", row.Identity)
	require.Equal(t, "20", row.Value.Remaining, "historical snapshot is retained but invalidated")
}
