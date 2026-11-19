package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

type AccountWalletSnapshot struct {
	AccountID int64
	InFlight  bool
	Identity  string
	Value     AccountWallet
}

// Snapshots contain only display data and an opaque identity digest. The lease
// prevents manual requests and the sole background owner from querying twice.
type AccountWalletSnapshotRepository interface {
	Get(context.Context, int64) (*AccountWalletSnapshot, error)
	Claim(ctx context.Context, id int64, identity, token string, scheduled bool) (bool, error)
	Complete(ctx context.Context, id int64, identity, token string, value AccountWallet) (bool, error)
	Release(ctx context.Context, id int64, token string) error
	Due(context.Context, int) ([]AccountWalletSnapshot, error)
	Discard(ctx context.Context, id int64, identity string) error
}

func (s *AccountWalletReader) cachedWallet(a *Account, snapshot *AccountWalletSnapshot) AccountWallet {
	if !walletAccountSupported(a) {
		return AccountWallet{Status: "unsupported"}
	}
	if snapshot == nil || snapshot.Identity != AccountWalletIdentity(a) {
		return AccountWallet{Status: "pending", QueryURL: accountWalletCustomURL(a)}
	}
	value := snapshot.Value
	value.Querying = snapshot.InFlight
	if value.CheckedAt != nil && time.Since(*value.CheckedAt) > AccountWalletRefreshInterval+time.Minute {
		value.Stale = true
	}
	return value
}

func retainWalletHistory(value, previous AccountWallet) AccountWallet {
	if value.Status != "ready" && previous.Remaining != "" && previous.CheckedAt != nil {
		value.Remaining, value.Unit, value.CheckedAt = previous.Remaining, previous.Unit, previous.CheckedAt
		value.AutoRefresh, value.NextCheckAt, value.LastAttemptAt = previous.AutoRefresh, previous.NextCheckAt, previous.LastAttemptAt
		if value.QueryURL == "" {
			value.QueryURL = previous.QueryURL
		}
		value.Stale = true
		value.Status = "error"
		if value.Error == "" {
			value.Error = "upstream_unavailable"
		}
	}
	return value
}

func (s *AccountWalletReader) readPersisted(ctx context.Context, a *Account, refresh, scheduled bool) AccountWallet {
	if !walletAccountSupported(a) {
		return AccountWallet{Status: "unsupported"}
	}
	snapshot, err := s.store.Get(ctx, a.ID)
	if err != nil {
		return walletFailure("wallet_snapshot_unavailable")
	}
	previous := s.cachedWallet(a, snapshot)
	if !refresh {
		return previous // Opening the page never waits for an upstream call.
	}
	if ctx.Err() != nil {
		return retainWalletHistory(walletRequestFailure(ctx, nil, "upstream_unavailable"), previous)
	}
	// Never spend the upstream query budget waiting in a local queue.
	release, ok := s.acquireWalletSlot(a)
	if !ok {
		return retainWalletHistory(walletFailure("refresh_in_progress_or_rate_limited"), previous)
	}
	defer release()
	identity, token := AccountWalletIdentity(a), uuid.NewString()
	claimed, err := s.store.Claim(ctx, a.ID, identity, token, scheduled)
	if err != nil {
		return retainWalletHistory(walletFailure("wallet_snapshot_unavailable"), previous)
	}
	if !claimed {
		latest, err := s.store.Get(ctx, a.ID)
		if err != nil {
			return retainWalletHistory(walletFailure("wallet_snapshot_unavailable"), previous)
		}
		value := s.cachedWallet(a, latest)
		if value.Status == "pending" {
			value.Error = "refresh_in_progress_or_rate_limited"
		}
		return value
	}
	defer func() {
		// A cancelled HTTP request or shutdown must release its lease without
		// turning a client cancellation into a persisted upstream failure.
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.store.Release(cleanup, a.ID, token)
	}()
	// Another process may have completed between the first read and this lease.
	// Retain its latest successful value if the new attempt fails.
	snapshot, err = s.store.Get(ctx, a.ID)
	if err != nil {
		return retainWalletHistory(walletFailure("wallet_snapshot_unavailable"), previous)
	}
	previous = s.cachedWallet(a, snapshot)
	call, cancel := context.WithTimeout(ctx, AccountWalletQueryTimeout)
	value := s.fetch(call, a)
	cancel()
	value.QueryURL = accountWalletCustomURL(a)
	if ctx.Err() != nil {
		return retainWalletHistory(walletRequestFailure(ctx, nil, "upstream_unavailable"), previous)
	}
	// Recheck identity before publishing any amount; configuration edits are
	// allowed while the network call is running. No old credential is persisted.
	current, err := s.accounts.GetByIDs(ctx, []int64{a.ID})
	if err != nil {
		return walletFailure("wallet_snapshot_unavailable")
	}
	if len(current) != 1 || current[0] == nil || AccountWalletIdentity(current[0]) != identity {
		return walletFailure("wallet_config_changed")
	}
	value = retainWalletHistory(value, previous)
	now := time.Now().UTC()
	value.LastAttemptAt = &now
	value.AutoRefresh = value.CheckedAt != nil && value.Remaining != ""
	if s.Manager != nil && !s.Manager.IsQueryChannel(ctx, a) {
		value.AutoRefresh = false
		value.NextCheckAt = nil
	}
	if value.AutoRefresh {
		next := now.Add(AccountWalletRefreshInterval)
		value.NextCheckAt = &next
	}
	saved, err := s.store.Complete(ctx, a.ID, identity, token, value)
	if err != nil {
		// Display the successful query honestly, but never claim it was scheduled.
		value.AutoRefresh, value.NextCheckAt = false, nil
		value.Error = "wallet_snapshot_unavailable"
		return value
	}
	if !saved {
		return walletFailure("wallet_config_changed")
	}
	return value
}

func (s *AccountWalletReader) Start() {
	if s.store == nil || s.accounts == nil || s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if s.Manager != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				s.Manager.Tick(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			s.refreshDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *AccountWalletReader) Stop() {
	s.mu.Lock()
	s.closing = true
	s.asyncCancel()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *AccountWalletReader) refreshDue(ctx context.Context) {
	read, cancel := context.WithTimeout(ctx, 5*time.Second)
	rows, err := s.store.Due(read, 100)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			logger.L().Warn("[AccountWalletRefresh] cannot read due snapshots")
		}
		return
	}
	// Use twenty workers, never one goroutine per account. Per-site gates keep
	// each upstream below four concurrent requests. Due times are
	// persisted individually, so refreshes stay staggered across restarts.
	jobs := make(chan AccountWalletSnapshot)
	var workers sync.WaitGroup
	for i := 0; i < accountWalletConcurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for row := range jobs {
				call, cancel := context.WithTimeout(ctx, AccountWalletOperationTimeout)
				accounts, err := s.accounts.GetByIDs(call, []int64{row.AccountID})
				if err == nil {
					if len(accounts) == 1 && accounts[0] != nil && s.Manager != nil && !s.Manager.IsQueryChannel(call, accounts[0]) {
						cancel()
						continue
					}
					if len(accounts) != 1 || accounts[0] == nil || !walletAccountSupported(accounts[0]) || AccountWalletIdentity(accounts[0]) != row.Identity {
						_ = s.store.Discard(call, row.AccountID, row.Identity)
					} else {
						s.readPersisted(call, accounts[0], true, true)
					}
				}
				cancel()
			}
		}()
	}
send:
	for _, row := range rows {
		select {
		case jobs <- row:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
}
