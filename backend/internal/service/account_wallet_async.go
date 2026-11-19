package service

import (
	"context"
	"crypto/hmac"
	"regexp"
	"time"
)

// Browser requests poll an identity-bound operation instead of holding an HTTP
// connection open across a proxy deadline. Only results, never credentials, are
// cached here; successful saved queries still use the durable snapshot store.
type walletAsyncJob struct {
	result  AccountWalletProbe
	err     error
	done    bool
	expires time.Time
}

var walletRequestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func ValidWalletRequestID(id string) bool { return walletRequestIDPattern.MatchString(id) }

func (s *AccountWalletReader) walletAsync(ctx context.Context, key string, pending AccountWalletProbe, work func(context.Context) (AccountWalletProbe, error)) (AccountWalletProbe, error) {
	if ctx.Err() != nil {
		return AccountWalletProbe{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return AccountWalletProbe{Wallet: walletFailure("upstream_unavailable")}, nil
	}
	for id, job := range s.asyncJobs {
		if job.done && time.Now().After(job.expires) {
			delete(s.asyncJobs, id)
		}
	}
	if job, ok := s.asyncJobs[key]; ok {
		return job.result, job.err
	}
	if s.asyncActive >= accountWalletConcurrency || len(s.asyncJobs) >= 512 {
		pending.Wallet.Querying = false
		pending.Wallet = retainWalletHistory(walletFailure("refresh_in_progress_or_rate_limited"), pending.Wallet)
		return pending, nil
	}
	pending.Wallet.Querying = true
	job := &walletAsyncJob{result: pending}
	s.asyncJobs[key] = job
	s.asyncActive++
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		call, cancel := context.WithTimeout(s.asyncCtx, AccountWalletOperationTimeout)
		defer cancel()
		result, err := work(call)
		s.mu.Lock()
		defer s.mu.Unlock()
		job.result, job.err, job.done, job.expires = result, err, true, time.Now().Add(10*time.Minute)
		s.asyncActive--
	}()
	return pending, nil
}

func (s *AccountWalletReader) ReadAsync(ctx context.Context, a *Account, requestID string) AccountWallet {
	if !ValidWalletRequestID(requestID) {
		return walletFailure("upstream_response_invalid")
	}
	if !walletAccountSupported(a) {
		return AccountWallet{Status: "unsupported"}
	}
	previous := AccountWallet{Status: "pending"}
	if s.store != nil {
		previous = s.Read(ctx, a, false)
	}
	scope, err := s.Scope(ctx, a)
	if err != nil {
		return AccountWallet{Status: "rebind_required", Error: "wallet_rebind_required"}
	}
	key := s.walletSignature("read", scope, requestID)
	result, err := s.walletAsync(ctx, key, AccountWalletProbe{Wallet: previous}, func(call context.Context) (AccountWalletProbe, error) {
		value := s.Read(call, a, true)
		// A scheduled query in another process may already own the lease.
		// Follow its persisted result instead of caching "querying" forever.
		for value.Querying {
			select {
			case <-call.Done():
				return AccountWalletProbe{Wallet: retainWalletHistory(walletRequestFailure(call, call.Err(), "upstream_unavailable"), value)}, nil
			case <-time.After(time.Second):
				value = s.Read(call, a, false)
			}
		}
		return AccountWalletProbe{Wallet: value}, nil
	})
	if err != nil {
		return walletRequestFailure(ctx, err, "upstream_unavailable")
	}
	return result.Wallet
}

// Explicit administrator verification of a prospective representative uses its
// own saved URL and key. Group resolution must not substitute the old channel.
func (s *AccountWalletReader) VerifyQueryChannelAsync(ctx context.Context, a *Account, requestID string) AccountWallet {
	if !ValidWalletRequestID(requestID) || !walletAccountSupported(a) {
		return walletFailure("upstream_response_invalid")
	}
	key := s.walletSignature("verify-channel", AccountWalletIdentity(a), requestID)
	result, err := s.walletAsync(ctx, key, AccountWalletProbe{Wallet: AccountWallet{Status: "pending"}}, func(call context.Context) (AccountWalletProbe, error) {
		value := s.readRaw(call, a, true)
		for value.Querying {
			select {
			case <-call.Done():
				return AccountWalletProbe{Wallet: retainWalletHistory(walletRequestFailure(call, call.Err(), "upstream_unavailable"), value)}, nil
			case <-time.After(time.Second):
				value = s.readRaw(call, a, false)
			}
		}
		return AccountWalletProbe{Wallet: value}, nil
	})
	if err != nil {
		return walletFailure("upstream_unavailable")
	}
	return result.Wallet
}

func (s *AccountWalletReader) ProbeConfigurationAsync(ctx context.Context, a *Account, rawURL, revision, requestID string) (AccountWalletProbe, error) {
	if !ValidWalletRequestID(requestID) || !hmac.Equal([]byte(revision), []byte(s.Configuration(a).Revision)) {
		return AccountWalletProbe{}, ErrWalletConfigChanged
	}
	if !walletAccountSupported(a) {
		return AccountWalletProbe{}, ErrWalletUnsupported
	}
	target, err := NormalizeAccountWalletURL(rawURL)
	if err != nil {
		return AccountWalletProbe{}, err
	}
	key := s.walletSignature("probe-job", AccountWalletIdentity(a), target, requestID)
	return s.walletAsync(ctx, key, AccountWalletProbe{URL: target, Wallet: AccountWallet{Status: "pending"}}, func(call context.Context) (AccountWalletProbe, error) {
		return s.ProbeConfiguration(call, a, target, revision)
	})
}
