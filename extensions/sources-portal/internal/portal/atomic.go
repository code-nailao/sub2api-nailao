package portal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

var ErrPersistenceUncertain = errors.New("persistence_uncertain_restart_required")

type committedWriteError struct{ cause error }

func (e *committedWriteError) Error() string { return ErrPersistenceUncertain.Error() }
func (e *committedWriteError) Unwrap() error { return e.cause }
func writeCommitted(err error) bool {
	var committed *committedWriteError
	return errors.As(err, &committed)
}
func writeJSONAtomic(path string, value any) error {
	return writeJSONAtomicWithSync(path, value, func(parent string) error {
		dir, e := os.Open(parent)
		if e != nil {
			return e
		}
		defer dir.Close()
		return dir.Sync()
	})
}
func writeJSONAtomicWithSync(path string, value any, syncParent func(string) error) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closed := f.Close()
	if e != nil {
		return e
	}
	if closed != nil {
		return closed
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	if e = syncParent(filepath.Dir(path)); e != nil {
		return &committedWriteError{cause: e}
	}
	return nil
}
