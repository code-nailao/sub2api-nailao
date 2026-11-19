package portal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Source struct {
	Querying      bool      `json:"querying,omitempty"`
	AlertPending  bool      `json:"alert_pending"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Origin        string    `json:"origin"`
	AccountID     int64     `json:"account_id"`
	Threshold     string    `json:"threshold"`
	ThresholdUnit string    `json:"threshold_unit"`
	LastBalance   *Balance  `json:"last_balance,omitempty"`
	LastCheckedAt time.Time `json:"last_checked_at"`
	NextCheckAt   time.Time `json:"next_check_at"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UpdatedBy     int64     `json:"updated_by"`
	Revision      int64     `json:"revision"`
}
type Notice struct {
	ID       string  `json:"id"`
	SourceID string  `json:"source_id"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Balance  Balance `json:"balance"`
}
type record struct {
	Source  Source  `json:"source"`
	Secret  string  `json:"encrypted_token"`
	Alarm   string  `json:"alarm"`
	Pending *Notice `json:"pending,omitempty"`
}
type diskState struct {
	Version int               `json:"version"`
	Records map[string]record `json:"records"`
}
type Store struct {
	mu       sync.Mutex
	path     string
	lock     *os.File
	aead     cipher.AEAD
	state    diskState
	bindings bindingState
	failed   error
}

// errReadOnlyCheck marks a store loaded by CheckStore: every write path sees a
// failed store and refuses, so a check can never persist anything.
var errReadOnlyCheck = errors.New("read_only_startup_check")

func OpenStore(dir string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption_key_must_be_32_bytes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "portal.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("store_already_open")
	}
	s, err := loadStore(dir, key)
	if err != nil {
		lock.Close()
		return nil, err
	}
	s.lock = lock
	return s, nil
}

// CheckStore reads and validates the state OpenStore would load, without
// taking the lock or creating anything. dir must already exist; the running
// owner may hold the lock, which is safe because files are replaced atomically.
// The returned store refuses every write.
func CheckStore(dir string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption_key_must_be_32_bytes")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("data_directory_required")
	}
	if err := checkCreatable(filepath.Join(dir, "portal.lock"), false); err != nil {
		return nil, err
	}
	s, err := loadStore(dir, key)
	if err != nil {
		return nil, err
	}
	s.failed = errReadOnlyCheck
	return s, nil
}

// checkCreatable fails when a path the normal start would create or open
// already exists with the wrong type. A missing path is not an error.
func checkCreatable(path string, dir bool) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if dir && !info.IsDir() || !dir && !info.Mode().IsRegular() {
		return errors.New("unexpected_state_file_type")
	}
	return nil
}

// loadStore is the read, decode and validate step shared by OpenStore and
// CheckStore.
func loadStore(dir string, key []byte) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "sources.json"), aead: aead, state: diskState{Version: 1, Records: map[string]record{}}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if json.Unmarshal(b, &s.state) != nil || s.state.Version != 1 || s.state.Records == nil {
			return nil, errors.New("invalid_store")
		}
		for _, r := range s.state.Records {
			if _, err = s.decrypt(r); err != nil {
				return nil, errors.New("store_key_mismatch")
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err = s.loadBindings(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	return s.lock.Close()
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (s *Store) encrypt(src Source, token string) string {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(token), []byte(src.ID+"\n"+src.Origin)))
}
func (s *Store) decrypt(r record) (string, error) {
	b, err := base64.StdEncoding.DecodeString(r.Secret)
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", errors.New("invalid_ciphertext")
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, b[:n], b[n:], []byte(r.Source.ID+"\n"+r.Source.Origin))
	return string(plain), err
}
func (s *Store) snapshot() map[string]record {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]record, len(s.state.Records))
	for k, v := range s.state.Records {
		result[k] = v
	}
	return result
}
func (s *Store) change(fn func(map[string]record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return ErrPersistenceUncertain
	}
	next := make(map[string]record, len(s.state.Records))
	for k, v := range s.state.Records {
		next[k] = v
	}
	if err := fn(next); err != nil {
		return err
	}
	nextState := diskState{Version: 1, Records: next}
	err := writeJSONAtomic(s.path, nextState)
	if err == nil || writeCommitted(err) {
		s.state = nextState
	}
	if err != nil {
		s.failed = err
	}
	return err
}
