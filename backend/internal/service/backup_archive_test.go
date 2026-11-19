//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanBackupFileParts_ReassemblesExactBytes(t *testing.T) {
	src := writeBackupArchiveFixture(t, []byte("0123456789abcdefg"))
	parts, err := planBackupFileParts(context.Background(), src, 5)
	require.NoError(t, err)
	require.Len(t, parts, 4)

	file, err := os.Open(src)
	require.NoError(t, err)
	defer file.Close()
	var got bytes.Buffer
	for i, part := range parts {
		require.Equal(t, i+1, part.Index)
		require.LessOrEqual(t, part.SizeBytes, int64(5))
		require.Equal(t, int64(i*5), part.Offset)
		data, readErr := io.ReadAll(io.NewSectionReader(file, part.Offset, part.SizeBytes))
		require.NoError(t, readErr)
		require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(data)), part.SHA256)
		got.Write(data)
	}
	require.Equal(t, []byte("0123456789abcdefg"), got.Bytes())
}

func TestPlanBackupFileParts_RejectsInvalidInput(t *testing.T) {
	src := writeBackupArchiveFixture(t, []byte("data"))

	_, err := planBackupFileParts(context.Background(), src, 0)
	require.Error(t, err)

	empty := writeBackupArchiveFixture(t, nil)
	_, err = planBackupFileParts(context.Background(), empty, 5)
	require.Error(t, err)

	_, err = planBackupFileParts(context.Background(), filepathForMissingBackupArchive(t), 5)
	require.Error(t, err)
}

func writeBackupArchiveFixture(t *testing.T, content []byte) string {
	t.Helper()
	path := filepathForBackupArchive(t)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func filepathForBackupArchive(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/archive.gz"
}

func filepathForMissingBackupArchive(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/missing.gz"
}

func TestPlanBackupFileParts_NoTemporaryCopies(t *testing.T) {
	src := writeBackupArchiveFixture(t, entropyBackupFixture(256*1024))
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	parts, err := planBackupFileParts(context.Background(), src, 17*1024)
	require.NoError(t, err)
	require.Greater(t, len(parts), 1)
	entries, err := os.ReadDir(temp)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPlanBackupFileParts_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := planBackupFileParts(ctx, writeBackupArchiveFixture(t, []byte("data")), 1)
	require.ErrorIs(t, err, context.Canceled)
}
