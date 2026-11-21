package processrole

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Status struct {
	InstanceID         string            `json:"instance_id"`
	Role               Role              `json:"role"`
	Ready              bool              `json:"ready"`
	Draining           bool              `json:"draining"`
	OwnsBackground     bool              `json:"owns_background"`
	ActiveHTTP         int               `json:"active_http"`
	ActiveImages       int               `json:"active_images"`
	Hijacked           int               `json:"hijacked"`
	UnresolvedImages   int               `json:"unresolved_images"`
	BackgroundServices map[string]string `json:"background_services"`
	Admission          *AdmissionStatus  `json:"admission,omitempty"`
	// Background owner only (handoff.go): what admin requests a draining
	// owner still answers, in-flight admin requests it would refuse while
	// draining (writes and side-effect reads), and cleanup steps that have not
	// finished.
	AdminDuringDrain                string   `json:"admin_during_drain,omitempty"`
	ActiveAdminWrites               int      `json:"active_admin_writes,omitempty"`
	Stopping                        []string `json:"stopping,omitempty"`
	ScheduledBackupRemainingSeconds *int     `json:"scheduled_backup_remaining_seconds,omitempty"`
}

func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	services := make(map[string]string, len(r.services))
	for k, v := range r.services {
		services[k] = v
	}
	status := Status{InstanceID: r.id, Role: r.config.Role, Ready: r.ready && !r.drain, Draining: r.drain,
		OwnsBackground: r.owner != nil, ActiveHTTP: r.http, ActiveImages: r.images,
		Hijacked: r.hijack, UnresolvedImages: r.failed, BackgroundServices: services}
	r.handoffStatusLocked(&status)
	if !r.scheduledBackupDeadline.IsZero() {
		// Zero still signals an active backup winding down after its deadline.
		remaining := max(time.Until(r.scheduledBackupDeadline), 0)
		seconds := int((remaining + time.Second - 1) / time.Second)
		status.ScheduledBackupRemainingSeconds = &seconds
	}
	if r.admission != nil {
		admission := r.admission.Status()
		status.Admission = &admission
	}
	return status
}

func (r *Runtime) Ready() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.drain {
		r.ready = true
	}
}

// Drain is sticky for this process generation. It changes readiness, not the
// handling of connections still owned by the previous Nginx worker generation.
func (r *Runtime) Drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drain = true
	r.signal()
}

func (r *Runtime) signal() {
	close(r.change)
	r.change = make(chan struct{})
}

func (r *Runtime) add(counter *int) func() {
	r.mu.Lock()
	*counter++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			*counter--
			r.signal()
		})
	}
}

// BeginImage runs before acknowledging 202, so drain cannot miss an accepted
// task between the HTTP handler returning and its goroutine starting.
// A failed result write remains an explicit drain blocker for operator review.
func (r *Runtime) BeginImage() func(persisted bool) {
	end := r.add(&r.images)
	var once sync.Once
	return func(persisted bool) {
		once.Do(func() {
			if !persisted {
				r.mu.Lock()
				r.failed++
				r.mu.Unlock()
			}
			end()
		})
	}
}

func (r *Runtime) Wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		idle := r.http == 0 && r.images == 0 && r.hijack == 0 && r.failed == 0
		changed := r.change
		gate := r.admission
		r.mu.Unlock()
		var admissionsChanged <-chan struct{}
		if gate != nil {
			gate.mu.Lock()
			idle = idle && gate.active == 0 && gate.state.PendingLegacy == 0 && !gate.switching && !gate.faulted
			admissionsChanged = gate.changed
			gate.mu.Unlock()
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-admissionsChanged:
		}
	}
}

func adminPath(path string) bool {
	// MCP dispatches management operations through a private router. Route the
	// transport to the unique owner before that dispatch, just like admin REST.
	// User consent/grant pages remain ordinary serving routes.
	if path == "/api/v1/admin" || strings.HasPrefix(path, "/api/v1/admin/") ||
		path == "/api/v1/mcp" || strings.HasPrefix(path, "/api/v1/mcp/") {
		return true
	}
	switch path {
	case "/.well-known/oauth-protected-resource/api/v1/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server":
		return true
	default:
		return false
	}
}

// These administrator operations read shared data, probe the configured upstream
// wallet without writes, or update display order in a database transaction. They never touch the process-local scheduler. Keep the
// same narrow method/path boundary as the reviewed Nginx bridge; the native
// admin authentication and feature guard still run in next.
func servingAdminRequest(req *http.Request) bool {
	const walletSettings = "/api/v1/admin/accounts/wallet-settings"
	if req.URL.Path == walletSettings {
		return req.Method == http.MethodGet || req.Method == http.MethodPut || req.Method == http.MethodDelete
	}
	if req.URL.Path == walletSettings+"/preview" || req.URL.Path == walletSettings+"/verify-channel" {
		return req.Method == http.MethodPost
	}
	if req.URL.Path == "/api/v1/admin/accounts/wallet-summaries" {
		return req.Method == http.MethodGet
	}
	if req.URL.Path == "/api/v1/admin/accounts/wallet-source-binding" {
		return req.Method == http.MethodPut
	}
	if req.URL.Path == "/api/v1/admin/accounts/wallet-source-deletion" {
		return req.Method == http.MethodPost
	}
	const walletConfig = "/api/v1/admin/accounts/balance-config"
	if req.URL.Path == walletConfig {
		return req.Method == http.MethodGet || req.Method == http.MethodPut
	}
	if req.URL.Path == walletConfig+"/test" {
		return req.Method == http.MethodPost
	}
	const base = "/api/v1/admin/channel-monitors"
	if req.Method == http.MethodPut {
		return req.URL.Path == base+"/sort-order"
	}
	if req.Method != http.MethodGet {
		return false
	}
	if req.URL.Path == "/api/v1/admin/accounts/balance-identities" || req.URL.Path == "/api/v1/admin/accounts/balance-auto" {
		return true
	}
	if req.URL.Path == base {
		return true
	}
	id, found := strings.CutPrefix(req.URL.Path, base+"/")
	if !found || len(id) == 0 || len(id) > 19 || id[0] < '1' || id[0] > '9' {
		return false
	}
	for i := 1; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// Wrap exposes a token-protected operator endpoint and preserves native HTTP
// semantics including streaming and upgraded connections. Nginx must pin admin
// operations that can schedule work to the unique background instance. Only the
// reviewed database-only operations may pass through a serving instance.
func (r *Runtime) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/internal/sources/") {
			r.control(w, req)
			return
		}
		if r.config.Role == Serving && adminPath(req.URL.Path) && !servingAdminRequest(req) {
			http.Error(w, "administration must use the background owner", http.StatusServiceUnavailable)
			return
		}
		if r.config.Role == Background && !adminPath(req.URL.Path) && req.URL.Path != "/health" {
			http.Error(w, "public requests must use a serving instance", http.StatusServiceUnavailable)
			return
		}
		if r.config.Role == Background && adminPath(req.URL.Path) {
			admitted, finish, ok := r.admitOwnerAdmin(req)
			if !ok {
				refuseDuringHandoff(w)
				return
			}
			defer finish()
			req = admitted
		}
		end := r.add(&r.http)
		defer end()
		next.ServeHTTP(&trackedWriter{ResponseWriter: w, runtime: r, ctx: req.Context()}, req)
	})
}

func (r *Runtime) control(w http.ResponseWriter, req *http.Request) {
	provided := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if r.config.Token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(r.config.Token)) != 1 {
		http.NotFound(w, req)
		return
	}
	switch {
	case req.URL.Path == "/internal/sources/runtime" && req.Method == http.MethodGet:
	case req.URL.Path == "/internal/sources/drain" && req.Method == http.MethodPost:
		var input struct {
			InstanceID string `json:"instance_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || input.InstanceID != r.id || decoder.Decode(&extra) != io.EOF {
			http.Error(w, "invalid drain instance identity", http.StatusConflict)
			return
		}
		r.Drain()
	case (req.URL.Path == "/internal/sources/admission" || req.URL.Path == "/internal/sources/admission/release") && req.Method == http.MethodPost && r.admission != nil:
		var input struct {
			InstanceID string `json:"instance_id"`
			Revision   uint64 `json:"revision"`
			Mode       string `json:"mode"`
			HoldToken  string `json:"hold_token"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.InstanceID != r.id {
			http.Error(w, "invalid admission transition identity", http.StatusConflict)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, "invalid admission transition", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
		defer cancel()
		if req.URL.Path == "/internal/sources/admission/release" {
			if input.Mode != AdmissionEnabled || r.releaseHeld == nil {
				http.Error(w, "invalid admission release", http.StatusConflict)
				return
			}
			count, err := r.admission.Release(input.Revision, input.HoldToken, r.releaseHeld)
			if err != nil {
				http.Error(w, "admission release blocked; inspect current runtime state", http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(struct {
				Released int    `json:"released"`
				Runtime  Status `json:"runtime"`
			}{count, r.Status()})
			return
		}
		if input.HoldToken != "" {
			http.Error(w, "transition does not accept a hold token", http.StatusBadRequest)
			return
		}
		if err := r.admission.Transition(ctx, input.Revision, input.Mode); err != nil {
			http.Error(w, "admission transition blocked; inspect current runtime state", http.StatusConflict)
			return
		}
	default:
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(r.Status())
}

type trackedWriter struct {
	http.ResponseWriter
	runtime *Runtime
	ctx     context.Context
}

func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *trackedWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// Gin and native keepalive writers still call this legacy interface. Preserve
// it so adding lifecycle tracking cannot panic a streaming image/text request.
func (w *trackedWriter) CloseNotify() <-chan bool {
	if notifier, ok := w.ResponseWriter.(interface{ CloseNotify() <-chan bool }); ok {
		return notifier.CloseNotify()
	}
	closed := make(chan bool, 1)
	context.AfterFunc(w.ctx, func() { closed <- true })
	return closed
}
func (w *trackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &trackedConn{Conn: conn, end: w.runtime.add(&w.runtime.hijack)}, rw, nil
}
func (w *trackedWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
func (w *trackedWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, src)
}

type trackedConn struct {
	net.Conn
	end func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.end()
	return err
}
