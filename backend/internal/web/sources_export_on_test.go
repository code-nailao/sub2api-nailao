//go:build embed

package web

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestSourcesExportAssetsIsAdditiveAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "index-old.js")
	require.NoError(t, os.WriteFile(older, []byte("old release"), 0o644))
	src := fstest.MapFS{
		"index-abc.js":      {Data: []byte("new js")},
		"nested/logo-1.svg": {Data: []byte("<svg/>")},
	}

	written, err := exportFS(src, dir)
	require.NoError(t, err)
	require.Equal(t, 2, written)
	got, err := os.ReadFile(filepath.Join(dir, "nested", "logo-1.svg"))
	require.NoError(t, err)
	require.Equal(t, "<svg/>", string(got))
	kept, err := os.ReadFile(older)
	require.NoError(t, err)
	require.Equal(t, "old release", string(kept), "assets of older releases are never removed")

	again, err := exportFS(src, dir)
	require.NoError(t, err)
	require.Zero(t, again, "identical files are skipped")
}

func TestSourcesExportAssetsRejectsConflictsAndRelativeDirs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index-abc.js"), []byte("different"), 0o644))
	_, err := exportFS(fstest.MapFS{"index-abc.js": {Data: []byte("new js")}}, dir)
	require.ErrorContains(t, err, "different content")

	_, err = exportFS(fstest.MapFS{}, "relative/dir")
	require.ErrorContains(t, err, "absolute directory")
}

func TestSourcesExportAssetsUsesEmbeddedBuild(t *testing.T) {
	written, err := ExportAssets(t.TempDir())
	require.NoError(t, err)
	require.Positive(t, written, "the embedded build must contain hashed assets")
}
