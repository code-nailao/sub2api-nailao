// Package processrole defines the small deployment boundary around native Main.
// It does not replace native request handling, billing, or background services.
package processrole

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Role string

const (
	Standalone Role = "standalone"
	Serving    Role = "serving"
	Background Role = "background"
)

type Config struct {
	Role    Role
	LockDir string
	Token   string
}

// ConfigFromEnv is called before constructing any service with startup effects.
// The default preserves a single native Main; split deployments are explicit.
func ConfigFromEnv() (Config, error) {
	c := Config{Role: Role(strings.TrimSpace(os.Getenv("SOURCES_PROCESS_ROLE"))), LockDir: os.Getenv("SOURCES_BACKGROUND_LOCK_DIR")}
	if c.Role == "" {
		c.Role = Standalone
	}
	if c.Role != Standalone && c.Role != Serving && c.Role != Background {
		return Config{}, errors.New("invalid SOURCES_PROCESS_ROLE")
	}
	path := os.Getenv("SOURCES_RUNTIME_TOKEN_FILE")
	if path != "" {
		// Pin the opened inode: Lstat followed by ReadFile can follow a replaced
		// symlink. Nonblocking open also prevents a FIFO from hanging startup.
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return Config{}, errors.New("runtime token must be an owner-only regular file")
		}
		file := os.NewFile(uintptr(fd), path)
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
			return Config{}, errors.New("runtime token must be an owner-only regular file")
		}
		raw, err := io.ReadAll(io.LimitReader(file, 8193))
		if err != nil {
			return Config{}, fmt.Errorf("read runtime token: %w", err)
		}
		c.Token = strings.TrimSpace(string(raw))
		if len(c.Token) < 32 || len(raw) > 8192 || strings.ContainsAny(c.Token, "\r\n") {
			return Config{}, errors.New("runtime token must contain 32 to 8192 bytes on one line")
		}
	}
	if c.Role != Standalone && (c.LockDir == "" || c.Token == "") {
		return Config{}, errors.New("split Main requires a shared background lock directory and runtime token")
	}
	if c.LockDir != "" && !filepath.IsAbs(c.LockDir) {
		return Config{}, errors.New("background lock directory must be absolute")
	}
	return c, nil
}

// The pointer is installed exactly once by main, before dependency construction.
// Unit tests of native services retain the standalone behavior by default.
var current = New(Config{Role: Standalone})

func Default() *Runtime       { return current }
func BackgroundEnabled() bool { return current.config.Role != Serving }

// StartBackground is for global scanners/runners only. Per-request billing,
// cache subscribers, queues, and synchronous audit protection run on every
// serving instance. Keeping the registry visible makes incomplete wiring fail
// deployment checks instead of hiding it behind Docker health.
func StartBackground(name string, start func()) {
	r := current
	r.mu.Lock()
	if r.config.Role == Serving {
		r.services[name] = "disabled"
		r.mu.Unlock()
		return
	}
	r.services[name] = "starting"
	r.mu.Unlock()
	start()
	r.mu.Lock()
	r.services[name] = "started"
	r.mu.Unlock()
}

func Initialize() (*Runtime, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	r := New(cfg)
	if err := r.acquireOwner(); err != nil {
		return nil, err
	}
	current = r
	return r, nil
}

type Runtime struct {
	config      Config
	id          string
	mu          sync.Mutex
	ready       bool
	drain       bool
	http        int
	images      int
	hijack      int
	failed      int
	change      chan struct{}
	owner       *os.File
	services    map[string]string
	admission   *AdmissionGate
	releaseHeld func(string) (int, error)
	// Background owner handoff (handoff.go): in-flight admin requests a
	// draining owner would refuse, cancel functions of upgraded admin
	// connections, and cleanup steps that have not finished yet.
	writes                  int
	upgrades                map[uint64]context.CancelFunc
	upgradeN                uint64
	stopping                map[string]int
	scheduledBackupDeadline time.Time
}

func (r *Runtime) AttachAdmission(g *AdmissionGate, release, inspect func(string) (int, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g.mu.Lock()
	g.inspectHeld = inspect
	g.mu.Unlock()
	r.admission = g
	r.releaseHeld = release
}

func New(cfg Config) *Runtime {
	if cfg.Role == "" {
		cfg.Role = Standalone
	}
	return &Runtime{config: cfg, id: randomID(), change: make(chan struct{}), services: make(map[string]string),
		upgrades: make(map[uint64]context.CancelFunc), stopping: make(map[string]int)}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cannot generate runtime identity")
	}
	return hex.EncodeToString(b[:])
}

// All owner-capable containers must bind the same host directory. The lock file
// must never be unlinked/replaced; its descriptor is held through all Cleanup.
// Old binaries without this protocol remain an explicit deployment gate.
func (r *Runtime) acquireOwner() error {
	if r.config.LockDir == "" {
		return nil
	}
	info, err := os.Lstat(r.config.LockDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("background lock directory must exist and be owner-only")
	}
	if r.config.Role == Serving {
		return nil
	}
	path := filepath.Join(r.config.LockDir, "native-background.lock")
	// #nosec G703 -- Fixed filename inside an operator-owned directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("open background lock: %w", err)
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return errors.New("background lock must be an owner-only regular file")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return errors.New("another native background process still owns the shared lock")
	}
	r.owner = f
	return nil
}

// closeOwner is for failed construction and isolated tests. Production keeps
// the descriptor until OS process exit, including goroutines without Stop hooks.
func (r *Runtime) closeOwner() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner == nil {
		return nil
	}
	err := r.owner.Close()
	r.owner = nil
	return err
}
