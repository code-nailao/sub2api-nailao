//go:build unit

package repository

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestS3BackupStore_UploadFile(t *testing.T) {
	var received []byte
	var receivedLength int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		receivedLength = r.ContentLength
		var err error
		received, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := newS3Client(context.Background(), s3ClientParams{
		Endpoint:        server.URL,
		Region:          "auto",
		AccessKeyID:     "test-ak",
		SecretAccessKey: "test-sk",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)

	content := []byte("streamed backup payload")
	filePath := t.TempDir() + "/part.gz"
	require.NoError(t, os.WriteFile(filePath, content, 0o600))

	store := &S3BackupStore{client: client, bucket: "backup-bucket"}
	size, err := store.UploadFile(context.Background(), "backup/part-1", filePath, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), size)
	require.Equal(t, int64(len(content)), receivedLength)
	require.Equal(t, content, received)
}

func TestS3BackupStore_UploadFileRange_RetriesExactSection(t *testing.T) {
	var bodies [][]byte
	var lengths []int64
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, data)
		lengths = append(lengths, r.ContentLength)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := newS3Client(context.Background(), s3ClientParams{Endpoint: server.URL, Region: "auto", AccessKeyID: "test-ak", SecretAccessKey: "test-sk", ForcePathStyle: true})
	require.NoError(t, err)
	// A sparse archive exercises offsets beyond 4GiB without allocating the archive.
	file, err := os.CreateTemp(t.TempDir(), "archive-*")
	require.NoError(t, err)
	offset := int64(5 * 1024 * 1024 * 1024)
	payload := []byte("bounded-section")
	_, err = file.WriteAt(append(append([]byte("prefix"), payload...), []byte("suffix")...), offset-6)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	store := &S3BackupStore{client: client, bucket: "backup-bucket"}
	size, err := store.UploadFileRange(context.Background(), "backup/part-2", file.Name(), offset, int64(len(payload)), "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), size)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2)
	for i := range bodies {
		require.Equal(t, payload, bodies[i])
		require.Equal(t, int64(len(payload)), lengths[i])
	}
}

func TestS3BackupStore_UploadFileRange_RejectsInvalidRange(t *testing.T) {
	file := t.TempDir() + "/archive"
	require.NoError(t, os.WriteFile(file, []byte("12345"), 0600))
	store := &S3BackupStore{} // Invalid ranges must fail before any network access.
	for _, r := range [][2]int64{{-1, 1}, {0, 0}, {0, -1}, {5, 1}, {6, 1}, {1, math.MaxInt64}, {math.MaxInt64, 1}} {
		_, err := store.UploadFileRange(context.Background(), "part", file, r[0], r[1], "application/octet-stream")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.UploadFileRange(ctx, "part", file, 0, 5, "application/octet-stream")
	require.ErrorIs(t, err, context.Canceled)
}

func TestS3BackupStore_UploadFileRange_BoundedMemory(t *testing.T) {
	const length = int64(64 * 1024 * 1024)
	var received int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("read stream: %v", err)
			w.WriteHeader(400)
			return
		}
		atomic.StoreInt64(&received, n)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := newS3Client(context.Background(), s3ClientParams{Endpoint: server.URL, Region: "auto", AccessKeyID: "test-ak", SecretAccessKey: "test-sk", ForcePathStyle: true})
	require.NoError(t, err)
	file, err := os.CreateTemp(t.TempDir(), "sparse-archive-*")
	require.NoError(t, err)
	require.NoError(t, file.Truncate(length+123))
	require.NoError(t, file.Close())
	store := &S3BackupStore{client: client, bucket: "backup-bucket"}
	// Run this test alone: TotalAlloc measures allocations by the whole process.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n, err := store.UploadFileRange(context.Background(), "backup/part", file.Name(), 123, length, "application/octet-stream")
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.Equal(t, length, n)
	require.Equal(t, length, atomic.LoadInt64(&received))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16*1024*1024), "64MiB range must be streamed, not buffered")
}
