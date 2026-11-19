//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type deadlineProbeDumper struct {
	*mockDumper
	delay time.Duration
}

func (d *deadlineProbeDumper) Dump(ctx context.Context) (io.ReadCloser, error) {
	delay := d.delay
	if delay == 0 {
		delay = 28 * time.Minute
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.mockDumper.Dump(ctx)
}

type deadlineProbeStore struct {
	*mockObjectStore
	calls int
	delay time.Duration
}

func (s *deadlineProbeStore) UploadFile(ctx context.Context, key, path, contentType string) (int64, error) {
	s.calls++
	delay := s.delay
	if delay == 0 {
		delay = 90 * time.Second
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return 0, fmt.Errorf("S3 PutObject file: %w", ctx.Err())
	}
	return s.mockObjectStore.UploadFile(ctx, key, path, contentType)
}

func (s *deadlineProbeStore) UploadFileRange(ctx context.Context, key, path string, offset, length int64, contentType string) (int64, error) {
	s.calls++
	delay := s.delay
	if delay == 0 {
		delay = 90 * time.Second
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return 0, fmt.Errorf("S3 PutObject file range: %w", ctx.Err())
	}
	return s.mockObjectStore.UploadFileRange(ctx, key, path, offset, length, contentType)
}

// A long dump must leave enough time to upload every part within the total limit.
// Virtual time reproduces the observed part-2 deadline without a real upload.
func TestBackupDeadline_DumpMustNotConsumeUploadBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := newMockSettingRepo()
		store := &deadlineProbeStore{mockObjectStore: newMockObjectStore()}
		dumper := &deadlineProbeDumper{mockDumper: &mockDumper{dumpData: entropyBackupFixture(1024)}}
		svc := newTestBackupService(repo, dumper, store.mockObjectStore)
		defer svc.bgCancel()
		svc.partSizeBytes = 256
		record := &BackupRecord{ID: "deadline-probe", Status: "running", StartedAt: time.Now().Format(time.RFC3339)}
		started := time.Now()
		svc.executeBackup(record, store, &BackupS3Config{Prefix: "backups/"})
		t.Logf("elapsed=%s upload_calls=%d status=%s error=%s", time.Since(started), store.calls, record.Status, record.ErrorMsg)
		require.Equal(t, "completed", record.Status, record.ErrorMsg)
		require.Len(t, store.objects, len(record.Parts))
	})
}

func TestBackupDeadline_StillStopsAtTwoHours(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := newMockSettingRepo()
		store := &deadlineProbeStore{mockObjectStore: newMockObjectStore(), delay: 2 * time.Minute}
		dumper := &deadlineProbeDumper{mockDumper: &mockDumper{dumpData: entropyBackupFixture(1024)}, delay: 119 * time.Minute}
		svc := newTestBackupService(repo, dumper, store.mockObjectStore)
		defer svc.bgCancel()
		svc.partSizeBytes = 256
		record := &BackupRecord{ID: "bounded-deadline-probe", Status: "running", StartedAt: time.Now().Format(time.RFC3339)}
		started := time.Now()
		svc.executeBackup(record, store, &BackupS3Config{Prefix: "backups/"})
		require.Equal(t, 2*time.Hour, time.Since(started))
		require.Equal(t, "failed", record.Status)
		require.Contains(t, record.ErrorMsg, "context deadline exceeded")
		require.Empty(t, store.objects)
	})
}

func TestBackupRecovery_PreservesRunningBackupWithinTwoHours(t *testing.T) {
	repo := newMockSettingRepo()
	store := newMockObjectStore()
	svc := newTestBackupService(repo, &mockDumper{}, store)
	record := &BackupRecord{ID: "one-hour-backup", Status: "running", StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339), S3Key: "backups/active-part"}
	store.objects[record.S3Key] = []byte("in-flight archive")
	require.NoError(t, svc.saveRecord(context.Background(), record))
	svc.recoverStaleRecords()
	actual, err := svc.GetBackupRecord(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, "running", actual.Status)
	require.Contains(t, store.objects, record.S3Key)
	require.Empty(t, store.deletedKeys)
}
