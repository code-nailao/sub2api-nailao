//go:build unit

package service

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/stretchr/testify/require"
)

// The deployment role is installed once before dependency construction. Exercise
// each role in a fresh process rather than mutate that global around other tests.
func sourcesServiceProcessRoles(t *testing.T, exercise func(*testing.T, processrole.Role)) {
	t.Helper()
	if role := os.Getenv("SUB2API_SERVICE_ROLE_TEST"); role != "" {
		_, err := processrole.Initialize()
		require.NoError(t, err)
		exercise(t, processrole.Role(role))
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, role := range []processrole.Role{processrole.Serving, processrole.Background, processrole.Standalone} {
		t.Run(string(role), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			tokenPath := filepath.Join(dir, "token")
			require.NoError(t, os.WriteFile(tokenPath, []byte(strings.Repeat("a", 48)), 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			name := strings.SplitN(t.Name(), "/", 2)[0]
			cmd := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(),
				"SUB2API_SERVICE_ROLE_TEST="+string(role),
				"SOURCES_PROCESS_ROLE="+string(role),
				"SOURCES_BACKGROUND_LOCK_DIR="+dir,
				"SOURCES_RUNTIME_TOKEN_FILE="+tokenPath,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

type sourcesOllamaLifecycleRepo struct {
	*ollama429LinkRepo
	scans       atomic.Int64
	scanRelease <-chan struct{}
}

func (r *sourcesOllamaLifecycleRepo) ListDueOllamaCloudUsageAccounts(context.Context, time.Time, time.Duration, time.Duration, int) ([]Account, error) {
	if r.scans.Add(1) == 3 {
		// The third scan stands in for a persistence operation already in flight.
		// Stop must join it, even after cancellation has been requested.
		<-r.scanRelease
	}
	return nil, nil
}

func TestOllamaCloudUsageProcessRoleLifecycle(t *testing.T) {
	sourcesServiceProcessRoles(t, func(t *testing.T, role processrole.Role) {
		synctest.Test(t, func(t *testing.T) {
			account := ollamaUsageAccount(801)
			account.Extra[OllamaCloudUsageSessionExtraKey] = "cipher:wos-session=secret"
			account.Extra[OllamaCloudUsageAutoRefreshExtraKey] = false
			scanRelease := make(chan struct{})
			callbackRelease := make(chan struct{})
			repo := &sourcesOllamaLifecycleRepo{
				ollama429LinkRepo: &ollama429LinkRepo{ollamaUsageTestRepo: &ollamaUsageTestRepo{
					upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{
						accounts: map[int64]*Account{account.ID: account},
					},
				}},
				scanRelease: scanRelease,
			}
			reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
			upstream := &ollamaUsageHTTPStub{body: ollamaCloudProbeUsageBody(100, reset.Format(time.RFC3339))}
			settings := NewSettingService(&upstreamBillingProbeSettingRepo{values: map[string]string{
				SettingKeyOllamaCloudUsageSettings: `{"enabled":true,"interval_minutes":60}`,
			}}, nil)
			cfg := &config.Config{}
			cfg.Totp.EncryptionKeyConfigured = true
			svc := ProvideOllamaCloudUsageService(repo, upstream, settings, ollamaUsageTestEncryptor{}, cfg, nil, nil)
			defer func() {
				for _, ch := range []chan struct{}{callbackRelease, scanRelease} {
					select {
					case <-ch:
					default:
						close(ch)
					}
				}
				svc.Stop()
			}()
			svc.Start() // Repeated startup must not create another scanner/prober.
			synctest.Wait()
			wantScans := int64(0)
			if role != processrole.Serving {
				wantScans = 1
			}
			require.Equal(t, wantScans, repo.scans.Load(), "only the owner scans all due accounts")

			blocker := &ollama429BlockerStub{}
			rateLimits := NewRateLimitService(repo, nil, nil, nil, nil)
			rateLimits.SetAccountRuntimeBlocker(blocker)
			rateLimits.SetOllamaCloudUsageProbeScheduler(svc)
			rateLimits.handle429(context.Background(), account, http.Header{}, nil)
			synctest.Wait()
			require.Equal(t, 1, repo.casUpdatedCount(), "the real request-triggered probe must persist the reset exactly once")
			require.Equal(t, reset, *repo.currentReset(account.ID))
			rec, ok := blocker.last()
			require.True(t, ok)
			require.Equal(t, "ollama_cloud_usage_429_probe", rec.reason)
			require.Equal(t, reset, rec.until)
			require.Equal(t, int64(1), upstream.calls.Load())

			for cycle := int64(2); cycle <= 3; cycle++ {
				time.Sleep(ollamaCloudUsageCycleInterval)
				synctest.Wait()
				require.Equal(t, wantScans*cycle, repo.scans.Load(), "serving must never run the global periodic scan")
			}

			callbackEntered := make(chan struct{})
			require.True(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(account.ID, func(int64, time.Time) {
				close(callbackEntered)
				<-callbackRelease
			}))
			synctest.Wait()
			select {
			case <-callbackEntered:
			default:
				t.Fatal("every role must run its request probe coordinator")
			}
			stopped := make(chan struct{})
			go func() { svc.Stop(); close(stopped) }()
			synctest.Wait()
			select {
			case <-stopped:
				t.Fatal("Stop returned while a probe callback was in flight")
			default:
			}
			close(callbackRelease)
			synctest.Wait()
			if role != processrole.Serving {
				select {
				case <-stopped:
					t.Fatal("owner Stop returned while its global scan was in flight")
				default:
				}
			}
			close(scanRelease)
			<-stopped
			require.False(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(account.ID, func(int64, time.Time) {}))
			svc.Start()
			synctest.Wait()
			require.False(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(account.ID, func(int64, time.Time) {}), "a stopped generation cannot restart")
		})
	})
}

func TestUserPlatformQuotaFlusherStopProcessRole(t *testing.T) {
	sourcesServiceProcessRoles(t, func(t *testing.T, role processrole.Role) {
		cache := &mockQuotaDirtyCache{
			popSequence: [][]UserPlatformQuotaKey{{{UserID: 42, Platform: PlatformOpenAI}}},
			getEntries:  []*UserPlatformQuotaCacheEntry{makeEntry(1.25, 3.5, 7.75)},
		}
		writer := &mockQuotaSnapshotWriter{}
		svc := newTestFlusher(cache, writer)
		wheel, err := NewTimingWheelService()
		require.NoError(t, err)
		defer wheel.Stop()
		svc.timingWheel = wheel
		svc.Stop()
		if role == processrole.Serving {
			require.Zero(t, cache.popCallIdx, "serving shutdown must leave the shared dirty queue for its owner")
			require.Empty(t, writer.receivedSnaps)
		} else {
			// Retain native standalone behavior, including a final flush when the
			// periodic timer was disabled/not started.
			require.Equal(t, 1, cache.popCallIdx)
			require.Len(t, writer.receivedSnaps, 1)
			require.Equal(t, int64(42), writer.receivedSnaps[0].UserID)
			require.Equal(t, 1.25, writer.receivedSnaps[0].DailyUsageUSD)
			require.Equal(t, 3.5, writer.receivedSnaps[0].WeeklyUsageUSD)
			require.Equal(t, 7.75, writer.receivedSnaps[0].MonthlyUsageUSD)
		}
	})
}
