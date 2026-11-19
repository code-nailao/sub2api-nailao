//go:build embed

package web

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// ExportAssets copies the embedded dist/assets tree into dir without deleting
// or overwriting anything: asset names carry content hashes, so a union of all
// releases lets pages rendered by an older release keep loading their assets
// after a switch. An existing file with different bytes is an error. It
// returns the number of files written.
func ExportAssets(dir string) (int, error) {
	assets, err := fs.Sub(frontendFS, "dist/assets")
	if err != nil {
		return 0, err
	}
	return exportFS(assets, dir)
}

func exportFS(src fs.FS, dir string) (int, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return 0, errors.New("asset export requires an absolute directory")
	}
	written := 0
	err := fs.WalkDir(src, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() || path.Base(name) != entry.Name() {
			return errors.New("unexpected embedded asset entry: " + name)
		}
		data, err := fs.ReadFile(src, name)
		if err != nil {
			return err
		}
		if existing, err := os.ReadFile(target); err == nil {
			if string(existing) != string(data) {
				return errors.New("asset already exists with different content: " + name)
			}
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".export-*")
		if err != nil {
			return err
		}
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return err
		}
		if err := tmp.Chmod(0o644); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
		if err := os.Rename(tmp.Name(), target); err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
		written++
		return nil
	})
	return written, err
}
