package processrole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
)

const (
	AdmissionLegacy  = "legacy-proxy"
	AdmissionHeld    = "producer-held"
	AdmissionEnabled = "producer"
)

// AdmissionState is an operator-prepared, durable admission journal. The
// token identifies held tasks; it is not an API credential. There is no timed
// automatic release and no transition back to a legacy writer after producers
// have started. Recovery can keep the old engine with the new producer.
type AdmissionState struct {
	Schema        int    `json:"schema"`
	Revision      uint64 `json:"revision"`
	Mode          string `json:"mode"`
	HoldToken     string `json:"hold_token"`
	PendingLegacy int    `json:"pending_legacy"`
}

type AdmissionStatus struct {
	AdmissionState
	Active    int  `json:"active"`
	Switching bool `json:"switching"`
	Faulted   bool `json:"faulted"`
}

type AdmissionGate struct {
	mu          sync.Mutex
	state       AdmissionState
	path        string
	lock        *os.File
	active      int
	switching   bool
	faulted     bool
	changed     chan struct{}
	inspectHeld func(string) (int, error)
}

type AdmissionLease struct {
	Mode, HoldToken string
	finish          func(bool)
}

// Finish(false) is only for an ambiguous legacy HTTP outcome. It leaves durable
// pending evidence that prevents the old/new admission handoff. Never retry the
// paid POST to make this counter go away.
func (l *AdmissionLease) Finish(known bool) {
	if l != nil && l.finish != nil {
		l.finish(known)
	}
}

var admissionTokenPattern = regexp.MustCompile(`^[a-f0-9]{32,64}$`)

func OpenAdmissionGate(path string) (*AdmissionGate, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("admission journal requires an absolute path")
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("admission journal requires an owner-only directory")
	}
	// #nosec G703 -- Explicit operator-owned migration state, never an HTTP path.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("another serving process owns this admission journal")
	}
	fail := func(err error) (*AdmissionGate, error) { _ = lock.Close(); return nil, err }
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(errors.New("admission journal must already exist"))
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return fail(errors.New("admission journal must be an owner-only regular file"))
	}
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	var state AdmissionState
	if err := decoder.Decode(&state); err != nil {
		return fail(errors.New("invalid admission journal"))
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || state.Schema != 1 || state.Revision == 0 || state.PendingLegacy < 0 || !admissionTokenPattern.MatchString(state.HoldToken) || (state.Mode != AdmissionLegacy && state.Mode != AdmissionHeld && state.Mode != AdmissionEnabled) || (state.Mode != AdmissionLegacy && state.PendingLegacy != 0) {
		return fail(errors.New("invalid admission journal state"))
	}
	return &AdmissionGate{path: path, lock: lock, state: state, changed: make(chan struct{})}, nil
}

func (g *AdmissionGate) Status() AdmissionStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return AdmissionStatus{g.state, g.active, g.switching, g.faulted}
}

func (g *AdmissionGate) signal() { close(g.changed); g.changed = make(chan struct{}) }

func (g *AdmissionGate) persist(next AdmissionState) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(g.path), ".admission-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), g.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(g.path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return err
	}
	g.state = next
	return nil
}

func (g *AdmissionGate) Begin(ctx context.Context) (*AdmissionLease, error) {
	for {
		g.mu.Lock()
		if g.faulted {
			g.mu.Unlock()
			return nil, errors.New("admission journal needs operator recovery")
		}
		if g.switching {
			changed := g.changed
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		mode, token := g.state.Mode, g.state.HoldToken
		if mode == AdmissionLegacy {
			next := g.state
			next.PendingLegacy++
			if err := g.persist(next); err != nil {
				g.faulted = true
				g.mu.Unlock()
				return nil, err
			}
		}
		g.active++
		g.mu.Unlock()
		var once sync.Once
		return &AdmissionLease{Mode: mode, HoldToken: token, finish: func(known bool) {
			once.Do(func() {
				g.mu.Lock()
				defer g.mu.Unlock()
				if mode == AdmissionLegacy && known {
					next := g.state
					next.PendingLegacy--
					if err := g.persist(next); err != nil {
						g.faulted = true
					}
				}
				g.active--
				g.signal()
			})
		}}, nil
	}
}

// Transition blocks new admissions while prior requests finish, then fsyncs the
// next mode before admitting any new request. A timed-out caller reads Status;
// it never guesses whether to replay the control mutation.
func (g *AdmissionGate) Transition(ctx context.Context, revision uint64, target string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.faulted || g.switching || g.state.Revision != revision {
		return errors.New("admission transition state changed")
	}
	repeatedHold := g.state.Mode == AdmissionEnabled && target == AdmissionHeld
	allowed := g.state.Mode == AdmissionLegacy && target == AdmissionHeld || g.state.Mode == AdmissionHeld && target == AdmissionEnabled || repeatedHold
	if !allowed {
		return errors.New("admission transition is not permitted")
	}
	g.switching = true
	defer func() { g.switching = false; g.signal() }()
	for g.active != 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			return ctx.Err()
		case <-changed:
			g.mu.Lock()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.faulted || g.state.PendingLegacy != 0 {
		return errors.New("unresolved legacy request prevents admission handoff")
	}
	if g.state.Revision == math.MaxUint64 {
		return errors.New("admission revision exhausted")
	}
	if repeatedHold {
		// Reusing this journal's token is safe only after the previous batch
		// is empty. The revision rejects delayed releases from earlier cycles.
		// A read error must never implicitly release an unfinished batch.
		if g.inspectHeld == nil {
			return errors.New("admission hold inventory is unavailable")
		}
		remaining, err := g.inspectHeld(g.state.HoldToken)
		if err != nil || remaining != 0 {
			return errors.New("previous admission hold is not confirmed empty")
		}
	}
	next := g.state
	next.Mode = target
	next.Revision++
	if err := g.persist(next); err != nil {
		g.faulted = true
		return fmt.Errorf("persist admission transition: %w", err)
	}
	return nil
}

// Release only visits this journal's held batch after the barrier has proved
// there can be no further held creates. The producer performs an idempotent,
// bounded CAS release; callers may continue until a batch reports zero.
func (g *AdmissionGate) Release(revision uint64, token string, release func(string) (int, error)) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.faulted || g.switching || g.state.Mode != AdmissionEnabled || g.state.PendingLegacy != 0 || g.state.Revision != revision || token != g.state.HoldToken || release == nil {
		return 0, errors.New("admission hold release is not permitted")
	}
	return release(token)
}

func (g *AdmissionGate) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 || g.switching {
		return errors.New("admissions still in progress")
	}
	g.faulted = true
	return g.lock.Close()
}
