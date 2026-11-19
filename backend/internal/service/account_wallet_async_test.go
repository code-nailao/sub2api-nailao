package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWalletNetworkDeadlineExpiresAtSixMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, account := walletFixture(walletTransport{call: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}})
		defer reader.Stop()
		start := time.Now()
		value := reader.Read(context.Background(), account, true)
		require.Equal(t, "upstream_timeout", value.Error)
		require.Equal(t, 360*time.Second, time.Since(start))
	})
}

func TestWalletAsyncOutlivesBrowserAndPublishesSnapshot(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	reader, account := walletFixture(walletTransport{call: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
		calls.Add(1)
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		require.InDelta(t, 360, time.Until(deadline).Seconds(), 1)
		close(started)
		select {
		case <-finish:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return walletResponse(`{"mode":"unrestricted","isValid":true,"balance":12.34,"unit":"USD"}`), nil
	}})
	store := &walletSnapshotFixture{}
	reader.store, reader.accounts = store, &walletAccountFixture{account: account}
	t.Cleanup(reader.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	first := reader.ReadAsync(ctx, account, "browser-request-0001")
	require.True(t, first.Querying)
	<-started
	cancel()
	for i := 0; i < 3; i++ {
		require.True(t, reader.ReadAsync(context.Background(), account, "browser-request-0001").Querying)
	}
	close(finish)
	var result AccountWallet
	require.Eventually(t, func() bool {
		result = reader.ReadAsync(context.Background(), account, "browser-request-0001")
		return !result.Querying
	}, time.Second, time.Millisecond)
	require.Equal(t, "12.34", result.Remaining)
	require.True(t, result.AutoRefresh)
	require.InDelta(t, 300, result.NextCheckAt.Sub(*result.LastAttemptAt).Seconds(), .01)
	require.Equal(t, result, reader.Read(context.Background(), account, false))
	require.EqualValues(t, 1, calls.Load(), "polls must never launch another upstream request")
	reader.Stop()
	require.Equal(t, "upstream_unavailable", reader.ReadAsync(context.Background(), account, "browser-request-0002").Error)
}

func TestWalletAsyncProbeStopAndIdentityIsolation(t *testing.T) {
	started := make(chan struct{})
	reader, account := walletFixture(walletTransport{call: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}})
	first, err := reader.ProbeConfigurationAsync(context.Background(), account, "https://wallet.example", reader.Configuration(account).Revision, "probe-request-0001")
	require.NoError(t, err)
	require.True(t, first.Wallet.Querying)
	require.Empty(t, first.VerificationToken)
	<-started
	changed := *account
	changed.Credentials = map[string]any{"api_key": "rotated", "base_url": "https://upstream.example"}
	_, err = reader.ProbeConfigurationAsync(context.Background(), &changed, "https://wallet.example", reader.Configuration(account).Revision, "probe-request-0001")
	require.ErrorIs(t, err, ErrWalletConfigChanged)
	done := make(chan struct{})
	go func() { reader.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel an asynchronous query")
	}
}

func TestWalletRepresentativeVerificationFollowsExistingLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, account := walletFixture(nil)
		defer reader.Stop()
		now := time.Now().UTC()
		store := &walletSnapshotFixture{token: "another-process", row: &AccountWalletSnapshot{AccountID: account.ID, Identity: AccountWalletIdentity(account), InFlight: true, Value: AccountWallet{Status: "ready", Remaining: "12", Unit: "USD", CheckedAt: &now}}}
		reader.store, reader.accounts = store, &walletAccountFixture{account: account}
		require.True(t, reader.VerifyQueryChannelAsync(context.Background(), account, "representative-request-1").Querying)
		synctest.Wait()
		store.mu.Lock()
		store.row.InFlight = false
		store.token = ""
		store.row.Value.Remaining = "25"
		store.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		result := reader.VerifyQueryChannelAsync(context.Background(), account, "representative-request-1")
		require.False(t, result.Querying)
		require.Equal(t, "25", result.Remaining)
	})
}
