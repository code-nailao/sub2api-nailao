package processrole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// A draining background owner keeps its listener open while its background
// services stop, so the admin pages stay up during a release handoff. It only
// answers reads: a write could hand work to a service that has already stopped
// or start a goroutine that process exit would cut short. Everything refused
// here is refused before any handler runs, so retrying it is always safe.
const (
	AdminDuringDrainReadOnly = "read-only"
	// HandoffRetryAfterSeconds is small on purpose: the refusal ends when the
	// next owner is ready, and nothing here can tell when that will be.
	HandoffRetryAfterSeconds = "5"
	handoffReason            = "BACKGROUND_HANDOFF_READ_ONLY"
	handoffMessage           = "A release is in progress: administration is temporarily read-only. Please retry shortly."
)

// Admin reads that wait for the next owner like writes do, from a review of
// every admin GET against the services that stop during the drain. "*" is one
// path segment. A read is refused when it depends on state a stopped service
// has closed, or when it reaches an upstream provider: those probe, may
// refresh and save OAuth credentials, write account state or start work that
// outlives the request.
var handoffRefusedReads = []struct {
	pattern string
	refused func(query url.Values) bool // nil: always
}{
	// The media engine closes its store when it stops (misleading 503).
	{"sources/image-tasks", nil},
	// A refresh takes a lease row, queries the upstream wallet and saves the
	// snapshot; without it the page shows the stored snapshot.
	{"accounts/balance-auto", func(q url.Values) bool { return q.Get("refresh") != "" }},
	// The default active source probes upstream, writes account state and
	// starts detached updates; passive reads stored data only.
	{"accounts/*/usage", func(q url.Values) bool { return q.Get("source") != "passive" }},
	{"accounts/*/claude/reset-credits", nil},
	{"accounts/*/models", nil},
	{"openai/accounts/*/quota", nil},
	{"grok/accounts/*/quota", nil},
	{"cn-providers/accounts/*/quota", nil},
	{"cn-providers/accounts/*/balance", nil},
}

func matchAdminPattern(path, pattern string) bool {
	rest, ok := strings.CutPrefix(path, "/api/v1/admin/")
	if !ok {
		return false
	}
	got, want := strings.Split(rest, "/"), strings.Split(pattern, "/")
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != got[i] || got[i] == "" {
			return false
		}
	}
	return true
}

func refusedReadDuringHandoff(req *http.Request) bool {
	for _, read := range handoffRefusedReads {
		if matchAdminPattern(req.URL.Path, read.pattern) {
			return read.refused == nil || read.refused(req.URL.Query())
		}
	}
	return false
}

func (r *Runtime) Role() Role { return r.config.Role }

func readMethod(method string) bool { return method == http.MethodGet || method == http.MethodHead }

func upgradeRequest(req *http.Request) bool { return req.Header.Get("Upgrade") != "" }

// readableDuringHandoff reports whether a draining owner still answers this
// admin request. The MCP transport dispatches management operations of any
// kind, and upgraded connections would outlive the listener.
func readableDuringHandoff(req *http.Request) bool {
	if !readMethod(req.Method) || upgradeRequest(req) {
		return false
	}
	path := req.URL.Path
	if path == "/api/v1/mcp" || strings.HasPrefix(path, "/api/v1/mcp/") {
		return false
	}
	return !refusedReadDuringHandoff(req)
}

// admitOwnerAdmin admits an admin request on the background owner. Every
// request a draining owner would refuse, other than an upgrade, is counted as
// a write: writes, MCP and the refused reads that probe upstream or write
// account state. The drain check and the count change under one lock, so
// WaitAdminWrites cannot miss one admitted just before Drain. Upgraded
// connections stay out of the count (they would hold it open) and instead get
// a context that CloseAdminUpgrades cancels; their handlers end on Done.
func (r *Runtime) admitOwnerAdmin(req *http.Request) (*http.Request, func(), bool) {
	readable := readableDuringHandoff(req)
	upgrade := upgradeRequest(req)
	write := !upgrade && !readable
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain && !readable {
		return nil, nil, false
	}
	if write {
		r.writes++
	}
	var key uint64
	var cancel context.CancelFunc
	if upgrade {
		var ctx context.Context
		ctx, cancel = context.WithCancel(req.Context())
		r.upgradeN++
		key = r.upgradeN
		r.upgrades[key] = cancel
		req = req.WithContext(ctx)
	}
	var once sync.Once
	return req, func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if write {
				r.writes--
			}
			if cancel != nil {
				delete(r.upgrades, key)
				cancel()
			}
			r.signal()
		})
	}, true
}

// WaitAdminWrites returns once every counted admin request (see
// admitOwnerAdmin) admitted before Drain has finished. After Drain no new ones
// are admitted, so the count only falls. There is no deadline, as for net/http
// Shutdown.
func (r *Runtime) WaitAdminWrites(ctx context.Context) error {
	for {
		r.mu.Lock()
		idle := r.writes == 0
		changed := r.change
		r.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// CloseAdminUpgrades cancels the request context of every upgraded admin
// connection. net/http Shutdown neither closes nor waits for hijacked
// connections, and an open one would hold Wait until the client goes away.
func (r *Runtime) CloseAdminUpgrades() {
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.upgrades))
	for _, cancel := range r.upgrades {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Stopping reports a cleanup step in the runtime status until the returned
// function runs, so the release tool can say what the old owner still waits
// for while it drains.
func Stopping(name string) func() {
	r := current
	r.mu.Lock()
	r.stopping[name]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.stopping[name]--; r.stopping[name] <= 0 {
				delete(r.stopping, name)
			}
		})
	}
}

// BeginScheduledBackup reports the accepted backup's existing deadline to the
// release tool. It returns nil when draining has already begun.
func (r *Runtime) BeginScheduledBackup(deadline time.Time) func() {
	r.mu.Lock()
	if r.drain {
		r.mu.Unlock()
		return nil
	}
	r.scheduledBackupDeadline = deadline
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.scheduledBackupDeadline = time.Time{}
		r.mu.Unlock()
	}
}

// handoffStatusLocked fills the handoff fields; r.mu must be held.
func (r *Runtime) handoffStatusLocked(status *Status) {
	if r.config.Role != Background {
		return
	}
	status.AdminDuringDrain = AdminDuringDrainReadOnly
	status.ActiveAdminWrites = r.writes
	if len(r.stopping) > 0 {
		status.Stopping = make([]string, 0, len(r.stopping))
		for name := range r.stopping {
			status.Stopping = append(status.Stopping, name)
		}
		sort.Strings(status.Stopping)
	}
}

// refuseDuringHandoff answers in the native error envelope so the admin pages
// show the message with their existing error handling.
func refuseDuringHandoff(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", HandoffRetryAfterSeconds)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}{http.StatusServiceUnavailable, handoffMessage, handoffReason})
}
