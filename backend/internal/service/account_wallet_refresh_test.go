package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type walletSnapshotFixture struct {
	mu          sync.Mutex
	row         *AccountWalletSnapshot
	token       string
	due         bool
	completeErr error
}

func (r *walletSnapshotFixture) Get(context.Context, int64) (*AccountWalletSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row == nil {
		return nil, nil
	}
	copy := *r.row
	return &copy, nil
}
func (r *walletSnapshotFixture) Claim(_ context.Context, id int64, identity, token string, scheduled bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token != "" || scheduled && (!r.due || r.row == nil || r.row.Identity != identity) {
		return false, nil
	}
	if r.row == nil || r.row.Identity != identity {
		r.row = &AccountWalletSnapshot{AccountID: id, Identity: identity, Value: AccountWallet{Status: "pending"}}
	}
	r.token = token
	return true, nil
}
func (r *walletSnapshotFixture) Complete(_ context.Context, _ int64, identity, token string, value AccountWallet) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completeErr != nil {
		return false, r.completeErr
	}
	if r.row.Identity != identity || r.token != token {
		return false, nil
	}
	r.row.Value, r.token, r.due = value, "", false
	return true, nil
}
func (r *walletSnapshotFixture) Release(_ context.Context, _ int64, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == token {
		r.token = ""
	}
	return nil
}
func (r *walletSnapshotFixture) Due(context.Context, int) ([]AccountWalletSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row != nil && r.row.Value.AutoRefresh && r.due {
		return []AccountWalletSnapshot{*r.row}, nil
	}
	return nil, nil
}
func (r *walletSnapshotFixture) Discard(_ context.Context, _ int64, identity string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row != nil && r.row.Identity == identity {
		r.row = nil
		r.due = false
	}
	return nil
}

type walletAccountFixture struct {
	AccountRepository
	account *Account
}

func (r *walletAccountFixture) GetByIDs(context.Context, []int64) ([]*Account, error) {
	if r.account == nil {
		return nil, nil
	}
	return []*Account{r.account}, nil
}

func TestAccountWalletDurableReadsAndSuccessOnlySchedule(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	reader, a := walletFixture(walletTransport{call: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
		calls.Add(1)
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		require.InDelta(t, 360, time.Until(deadline).Seconds(), 1)
		if fail.Load() {
			return nil, context.DeadlineExceeded
		}
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":0,"unit":"USD"}`), nil
	}})
	store := &walletSnapshotFixture{}
	reader.store, reader.accounts = store, &walletAccountFixture{account: a}
	ctx := context.Background()
	require.Equal(t, "pending", reader.Read(ctx, a, false).Status)
	require.Zero(t, calls.Load(), "opening the page must never query upstream")
	fail.Store(true)
	firstFailure := reader.Read(ctx, a, true)
	require.Equal(t, "upstream_timeout", firstFailure.Error)
	require.False(t, firstFailure.AutoRefresh)
	require.Nil(t, firstFailure.NextCheckAt)
	store.due = true
	reader.refreshDue(ctx)
	require.EqualValues(t, 1, calls.Load(), "never-successful accounts are not polled")
	fail.Store(false)
	first := reader.Read(ctx, a, true)
	require.Equal(t, "0", first.Remaining)
	require.True(t, first.AutoRefresh, "zero is a successful balance")
	require.InDelta(t, 300, first.NextCheckAt.Sub(*first.LastAttemptAt).Seconds(), 0.01)
	// A new process reads the same store with no memory cache or network call.
	other := NewAccountWalletReader(reader.test)
	other.store, other.accounts = store, reader.accounts
	require.Equal(t, first, other.Read(ctx, a, false))
	require.EqualValues(t, 2, calls.Load())
	busy := retainWalletHistory(walletFailure("refresh_in_progress_or_rate_limited"), first)
	require.True(t, busy.AutoRefresh, "a busy manual refresh must not appear to disable the schedule")
	require.Equal(t, first.NextCheckAt, busy.NextCheckAt)
	fail.Store(true)
	store.due = true
	other.refreshDue(ctx)
	stale := reader.Read(ctx, a, false)
	require.Equal(t, "upstream_timeout", stale.Error)
	require.True(t, stale.Stale)
	require.True(t, stale.AutoRefresh)
	require.Equal(t, first.CheckedAt, stale.CheckedAt)
	require.Equal(t, "0", stale.Remaining)
	require.EqualValues(t, 3, calls.Load())
	// Identity changes invalidate both the amount and automatic enrollment.
	a.Credentials["api_key"] = "rotated"
	require.Empty(t, reader.Read(ctx, a, false).Remaining)
	store.due = true
	reader.refreshDue(ctx)
	require.Nil(t, store.row)
	require.EqualValues(t, 3, calls.Load())
}

func TestAccountWalletLeasePreventsCrossProcessDuplicateAndStalePublish(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		close(started)
		<-finish
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":15,"unit":"USD"}`), nil
	}})
	store := &walletSnapshotFixture{}
	accounts := &walletAccountFixture{account: a}
	reader.store, reader.accounts = store, accounts
	other := NewAccountWalletReader(reader.test)
	other.store, other.accounts = store, accounts
	result := make(chan AccountWallet, 1)
	go func() { result <- reader.Read(context.Background(), a, true) }()
	<-started
	duplicate := other.Read(context.Background(), a, true)
	require.Equal(t, "refresh_in_progress_or_rate_limited", duplicate.Error)
	changed := *a
	changed.Credentials = map[string]any{"api_key": "new", "base_url": "https://upstream.example"}
	accounts.account = &changed
	close(finish)
	require.Equal(t, "wallet_config_changed", (<-result).Error)
	require.Empty(t, store.row.Value.Remaining)
	require.Empty(t, store.token)
}

type walletTimeoutBody struct{}

func (walletTimeoutBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (walletTimeoutBody) Close() error             { return nil }

func TestAccountWalletBodyTimeoutAndClientCancellationAreDifferent(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: walletTimeoutBody{}}, nil
	}})
	require.Equal(t, "upstream_timeout", reader.Read(context.Background(), a, true).Error)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, "upstream_unavailable", walletRequestFailure(ctx, context.Canceled, "upstream_unavailable").Error)
}

func TestAccountWalletPersistenceFailureDoesNotClaimSchedule(t *testing.T) {
	reader, a := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":10,"unit":"USD"}`), nil
	}})
	reader.store = &walletSnapshotFixture{completeErr: errors.New("database unavailable")}
	reader.accounts = &walletAccountFixture{account: a}
	v := reader.Read(context.Background(), a, true)
	require.Equal(t, "10", v.Remaining)
	require.Equal(t, "wallet_snapshot_unavailable", v.Error)
	require.False(t, v.AutoRefresh)
	require.Nil(t, v.NextCheckAt)
}

func TestAccountWalletTwentyTotalAndFourPerSite(t *testing.T) {
	for _, sameSite := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-site-%v", sameSite), func(t *testing.T) {
			limit := 20
			if sameSite {
				limit = 4
			}
			started, release := make(chan struct{}, limit), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			reader, template := walletFixture(walletTransport{call: func(*http.Request, string, int64) (*http.Response, error) {
				started <- struct{}{}
				<-release
				return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":1,"unit":"USD"}`), nil
			}})
			account := func(id int) *Account {
				copy := *template
				copy.ID = int64(id)
				site := fmt.Sprintf("https://wallet-%d.example", id)
				if sameSite {
					site = "https://same-wallet.example"
				}
				copy.Credentials = map[string]any{"base_url": site, "api_key": "fixture"}
				return &copy
			}
			var done sync.WaitGroup
			for id := 1; id <= limit; id++ {
				done.Add(1)
				go func(a *Account) { defer done.Done(); reader.Read(context.Background(), a, true) }(account(id))
			}
			for i := 0; i < limit; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("expected concurrency was not available")
				}
			}
			v := reader.Read(context.Background(), account(limit+1), true)
			require.Equal(t, "refresh_in_progress_or_rate_limited", v.Error)
			once.Do(func() { close(release) })
			done.Wait()
			require.Empty(t, reader.siteSlots)
		})
	}
}

func TestAccountWalletWorkerStopPreservesLastSuccess(t *testing.T) {
	started := make(chan struct{})
	var blocked atomic.Bool
	reader, a := walletFixture(walletTransport{call: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
		if blocked.Load() {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":10,"unit":"USD"}`), nil
	}})
	store := &walletSnapshotFixture{}
	reader.store, reader.accounts = store, &walletAccountFixture{account: a}
	first := reader.Read(context.Background(), a, true)
	store.due = true
	blocked.Store(true)
	reader.Start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	reader.Stop()
	require.Equal(t, first, reader.Read(context.Background(), a, false))
	require.Empty(t, store.token)
}
