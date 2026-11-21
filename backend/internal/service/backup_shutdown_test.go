//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackupService_ScheduledBackupShutdown(t *testing.T) {
	for _, tt := range []struct {
		name      string
		dumpDelay time.Duration
	}{
		{name: "finish during shutdown wait", dumpDelay: 4 * time.Minute},
		{name: "finish after shutdown wait expires", dumpDelay: 6 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				startedAt := time.Now()
				repo := newMockSettingRepo()
				seedS3Config(t, repo)
				store := newMockObjectStore()
				dumper := &deadlineProbeDumper{mockDumper: &mockDumper{dumpData: []byte("fixture dump")}, delay: tt.dumpDelay}
				old := newTestBackupService(repo, dumper, store)
				go old.runScheduledBackup()
				synctest.Wait()

				stopped := make(chan struct{})
				go func() { old.Stop(); close(stopped) }()
				synctest.Wait()
				select {
				case <-stopped:
					t.Fatal("Stop returned while the backup was collecting the dump")
				default:
				}
				<-stopped
				require.Equal(t, tt.dumpDelay, time.Since(startedAt), "Stop must wait for the dump")
				records, err := old.ListBackups(ctx)
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, "completed", records[0].Status)
				require.Equal(t, "scheduled", records[0].TriggeredBy)
				require.Contains(t, store.objects, records[0].S3Key)
			})
		})
	}
}

type shutdownRetentionStore struct {
	*mockObjectStore
	cleanupStarted chan struct{}
}

func (s *shutdownRetentionStore) Delete(ctx context.Context, _ string) error {
	close(s.cleanupStarted)
	<-ctx.Done()
	return ctx.Err()
}

func TestBackupService_ShutdownCancelsOnlyRetentionAfterCompletedBackup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		repo := newMockSettingRepo()
		seedS3Config(t, repo)
		require.NoError(t, repo.Set(ctx, settingKeyBackupSchedule, `{"retain_count":1}`))
		store := &shutdownRetentionStore{mockObjectStore: newMockObjectStore(), cleanupStarted: make(chan struct{})}
		svc := newTestBackupService(repo, &mockDumper{dumpData: []byte("fixture dump")}, store.mockObjectStore)
		svc.storeFactory = func(context.Context, *BackupS3Config) (BackupObjectStore, error) { return store, nil }
		expired := BackupRecord{ID: "expired-fixture", Status: "completed", S3Key: "expired-fixture",
			StartedAt: time.Now().Add(-24 * time.Hour).Format(time.RFC3339)}
		require.NoError(t, svc.saveRecord(ctx, &expired))
		go svc.runScheduledBackup()
		<-store.cleanupStarted

		startedAt := time.Now()
		svc.Stop()
		require.Equal(t, 5*time.Minute, time.Since(startedAt))
		records, err := svc.ListBackups(ctx)
		require.NoError(t, err)
		require.Len(t, records, 2, "interrupted retention must keep the expired record for next time")
		require.Equal(t, "completed", records[0].Status)
		require.Equal(t, "scheduled", records[0].TriggeredBy)
		require.Contains(t, store.objects, records[0].S3Key)
	})
}
