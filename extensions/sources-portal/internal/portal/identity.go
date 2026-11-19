package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Identity struct {
	ID       int64    `json:"id"`
	Role     string   `json:"role"`
	Status   string   `json:"status"`
	Username string   `json:"username"`
	Balance  *float64 `json:"balance"`
}
type identityKey struct{}

var mainKeyPath = regexp.MustCompile(`^/api/v1/keys/[1-9][0-9]*$`)
var mainKeyNamePath = regexp.MustCompile(`^/api/v1/keys/[1-9][0-9]*/name$`)
var mainGroupModelsPath = regexp.MustCompile(`^/api/v1/admin/groups/[1-9][0-9]*/model-allowlist-candidates$`)

const walletSourceDeletionPath = "/api/v1/admin/accounts/wallet-source-deletion"
const mainGroupListPath = "/api/v1/admin/groups/all"

// studioRegistryReadPath is the two administrator reads behind the workbench
// group settings: the active group list and one group's model candidates. Main
// checks the administrator role; neither path writes anything.
func studioRegistryReadPath(u *url.URL) bool {
	return u.RawPath == "" && u.RawQuery == "" && (u.Path == mainGroupListPath || mainGroupModelsPath.MatchString(u.Path))
}

// mainAPI accepts only fixed internal paths constructed by this service. Credentials
// never follow redirects, and forwarded client IP is accepted only from configured proxy peers.
func (s *Server) mainAPI(r *http.Request, method, path string, body any, idempotency string, target any) error {
	u, parseErr := url.Parse(path)
	allowed := parseErr == nil && !u.IsAbs() && u.Host == "" && u.Fragment == "" && ((method == http.MethodPost && path == walletSourceDeletionPath) || managedWalletPath(method, path) || (method == "GET" && (balanceIdentityPath(u) || studioRegistryReadPath(u) || u.Path == "/api/v1/auth/me" || u.Path == "/api/v1/groups/available" || u.Path == "/api/v1/keys" || mainKeyPath.MatchString(u.Path))) || (method == "POST" && u.Path == "/api/v1/keys") || (method == "PUT" && mainKeyPath.MatchString(u.Path)) || (method == http.MethodPatch && u.RawPath == "" && u.RawQuery == "" && mainKeyNamePath.MatchString(u.Path)))
	if !allowed {
		return ErrCredential
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var buf io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		buf = bytes.NewReader(b)
	}
	origin := s.SubOrigin
	if s.InternalSubOrigin != "" {
		origin = s.InternalSubOrigin
	}
	req, e := http.NewRequestWithContext(ctx, method, origin+path, buf)
	if e != nil {
		return ErrTransport
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("User-Agent", r.UserAgent())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	if e = s.forwardClient(r, req); e != nil {
		return e
	}
	// Main owns session signature, expiry, revocation and optional IP/UA binding.
	// A bnd claim can exist while Main has binding disabled; checking it here
	// would reject native-valid sessions after a network or browser change.
	client := *s.AuthClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		return ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return ErrCredential
	}
	if res.StatusCode == 404 {
		return ErrNotFound
	}
	if res.StatusCode == 409 {
		if method == http.MethodPost && path == walletSourceDeletionPath {
			raw, err := io.ReadAll(io.LimitReader(res.Body, 4097))
			if err != nil || len(raw) > 4096 {
				return ErrResponse
			}
			var failure struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal(raw, &failure) == nil && failure.Reason == "wallet_source_linked" {
				return ErrSourceLinked
			}
		}
		return ErrConflict
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return ErrTransport
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return ErrResponse
	}
	var out struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil || out.Code == nil || *out.Code != 0 {
		return ErrResponse
	}
	if target != nil && json.Unmarshal(out.Data, target) != nil {
		return ErrResponse
	}
	return nil
}

// forwardClient passes the browser's address to Main. A forwarded address is
// believed only when the direct peer is a configured proxy.
func (s *Server) forwardClient(r, req *http.Request) error {
	if len(s.TrustedProxies) == 0 {
		return nil
	}
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return ErrCredential
	}
	peer, e := netip.ParseAddr(host)
	if e != nil {
		return ErrCredential
	}
	trusted := false
	for _, p := range s.TrustedProxies {
		if p.Contains(peer.Unmap()) {
			trusted = true
		}
	}
	clientIP := host
	if trusted {
		v := r.Header.Get("X-Forwarded-For")
		if v != "" {
			a, e := netip.ParseAddr(v)
			if e != nil {
				return ErrCredential
			}
			clientIP = a.Unmap().String()
		}
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	return nil
}

// gatewayModels lists the models one API key can see under a gateway prefix of
// Main. The prefix has passed studioCustomPrefix and the key belongs to the
// signed-in user; the key is neither stored nor logged here.
func (s *Server) gatewayModels(r *http.Request, key, prefix string) ([]string, error) {
	if key == "" || len(key) > 8192 || strings.ContainsAny(key, " \r\n") || !studioCustomPrefix.MatchString(prefix) {
		return nil, ErrCredential
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	origin := s.SubOrigin
	if s.InternalSubOrigin != "" {
		origin = s.InternalSubOrigin
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, origin+prefix+"/models", nil)
	if e != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", r.UserAgent())
	req.Header.Set("Accept", "application/json")
	if e = s.forwardClient(r, req); e != nil {
		return nil, e
	}
	client := *s.AuthClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		return nil, ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, ErrCredential
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, ErrTransport
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, ErrResponse
	}
	// The OpenAI-style list has data[].id; the Gemini-style one has
	// models[].name in the form models/<id>.
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, ErrResponse
	}
	ids := make([]string, 0, len(out.Data)+len(out.Models))
	for _, item := range out.Data {
		ids = append(ids, item.ID)
	}
	for _, item := range out.Models {
		ids = append(ids, strings.TrimPrefix(item.Name, "models/"))
	}
	models := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		// An ID the registry would refuse is left out rather than failing the list.
		if len(models) < 1000 && studioModelID.MatchString(id) && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	return models, nil
}

func (s *Server) identity(r *http.Request) (Identity, error) {
	header := r.Header.Get("Authorization")
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || token == header || len(token) > 16384 || strings.ContainsAny(token, "\r\n") {
		return Identity{}, errors.New("admin_login_required")
	}
	var user Identity
	if e := s.mainAPI(r, "GET", "/api/v1/auth/me", nil, "", &user); e != nil {
		if errors.Is(e, ErrCredential) {
			return Identity{}, errors.New("admin_login_required")
		}
		return Identity{}, errors.New("main_identity_unavailable")
	}
	if user.ID <= 0 || user.Status != "active" {
		return Identity{}, errors.New("admin_login_required")
	}
	return user, nil
}

// Only the exact administrator read path and bounded numeric IDs are forwarded.
func balanceIdentityPath(u *url.URL) bool {
	if u.Path != "/api/v1/admin/accounts/balance-identities" || u.RawPath != "" {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 1 || len(query["ids"]) != 1 {
		return false
	}
	_, err = parseBalanceIDs(query.Get("ids"))
	return err == nil
}
