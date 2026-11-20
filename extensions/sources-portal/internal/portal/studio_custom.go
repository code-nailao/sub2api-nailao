package portal

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A custom connection lets a user reach the workbench with one of their own
// API keys when their group is not registered. The user chooses the key, the
// interface and an allowed API subdomain; paths are derived by the server. Only the
// key's ID is stored, as for a group authorization, never the key.

var (
	ErrStudioCustom     = errors.New("invalid_custom_connection")
	errStudioCustomNone = errors.New("custom_connection_not_configured")
)

// A prefix is one to four plain segments, so it cannot carry a scheme, a host,
// a query, a parent step or an encoded character.
var studioCustomPrefix = regexp.MustCompile(`^(/[A-Za-z0-9][A-Za-z0-9._~-]{0,63}){1,4}$`)

type StudioCustomBinding struct {
	KeyID    int64  `json:"key_id"`
	Protocol string `json:"protocol"`
	APILabel string `json:"api_label,omitempty"`
	// Prefix is the gateway prefix the interface is served under, such as /v1.
	Prefix    string    `json:"prefix"`
	Models    []string  `json:"models"`
	UpdatedAt time.Time `json:"updated_at"`
}

// studioCustomDefaults is the interface and prefix Main serves images under
// for a group of this platform.
func studioCustomDefaults(platform string) (protocol, prefix string) {
	switch platform {
	case "grok":
		return "grok_image", "/v1"
	case "gemini":
		return "gemini_native", "/v1beta"
	case "antigravity":
		return "gemini_native", "/antigravity/v1beta"
	}
	return "openai_images", "/v1"
}

func studioCustomBinding(in StudioCustomBinding) (StudioCustomBinding, error) {
	models, ok := studioModels(in.Models)
	out := StudioCustomBinding{KeyID: in.KeyID, Protocol: in.Protocol, Prefix: in.Prefix, Models: models, APILabel: in.APILabel}
	if in.KeyID < 1 || !ok || in.APILabel != "" && !studioAPILabel.MatchString(in.APILabel) {
		return out, ErrStudioCustom
	}
	switch in.Protocol {
	case "gemini_native":
		if out.Prefix == "" {
			out.Prefix = "/v1beta"
		}
	case "openai_images", "grok_image":
		if out.Prefix == "" {
			out.Prefix = "/v1"
		}
		// The Studio client always addresses this interface below a /v1 segment.
		if !strings.HasSuffix(out.Prefix, "/v1") {
			return out, ErrStudioCustom
		}
	default:
		return out, ErrStudioCustom
	}
	if len(out.Prefix) > 128 || !studioCustomPrefix.MatchString(out.Prefix) {
		return out, ErrStudioCustom
	}
	return out, nil
}

// studioCustomTask is what the client is told to call: a base path to add to
// the API origin and the request contract. The two clients build addresses
// differently, so the same binding is expressed for the one that asked.
func studioCustomTask(b StudioCustomBinding, canvas bool) (string, map[string]any) {
	if b.Protocol == "gemini_native" {
		path := b.Prefix + "/models/{model}:generateContent"
		return "", map[string]any{"engine": "sub2api", "modes": []string{"sync"}, "createPath": path, "editPath": path, "resultKind": "image"}
	}
	if b.Prefix == "/v1" {
		return "", studioTask("sub2api", b.Protocol)
	}
	if canvas {
		// Canvas runs tasks only on the exact /v1 task path; under another
		// prefix it is given the plain interface of that prefix.
		return "", map[string]any{"engine": "sub2api", "modes": []string{"sync"}, "createPath": b.Prefix + "/images/generations", "editPath": b.Prefix + "/images/edits", "resultKind": "image"}
	}
	// Studio appends the paths to the base address and recognizes the task
	// interface by its path without a prefix.
	return b.Prefix, map[string]any{"engine": "sub2api", "modes": []string{"async"}, "createPath": "/images/generations/async", "editPath": "/images/edits/async", "statusPathTemplate": "/images/tasks/{task_id}", "resultKind": "image"}
}

func (s *StudioService) loadCustom() error {
	b, e := os.ReadFile(s.customPath)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	stored := map[string]StudioCustomBinding{}
	if json.Unmarshal(b, &stored) != nil {
		return ErrStudioCustom
	}
	for index, binding := range stored {
		user, e := strconv.ParseInt(index, 10, 64)
		normal, invalid := studioCustomBinding(binding)
		if e != nil || user < 1 || invalid != nil {
			return ErrStudioCustom
		}
		normal.UpdatedAt = binding.UpdatedAt
		stored[index] = normal
	}
	s.custom = stored
	return nil
}

func (s *StudioService) customBinding(user int64) (StudioCustomBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.custom[strconv.FormatInt(user, 10)]
	return binding, ok
}

// saveCustom stores one user's binding; nil removes it.
func (s *StudioService) saveCustom(user int64, binding *StudioCustomBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.customFailed {
		return ErrPersistenceUncertain
	}
	next := make(map[string]StudioCustomBinding, len(s.custom)+1)
	for index, stored := range s.custom {
		next[index] = stored
	}
	index := strconv.FormatInt(user, 10)
	if binding == nil {
		if _, ok := next[index]; !ok {
			return nil
		}
		delete(next, index)
	} else {
		next[index] = *binding
	}
	e := writeJSONAtomic(s.customPath, next)
	if e != nil && !writeCommitted(e) {
		return e
	}
	s.custom = next
	if e != nil {
		s.customFailed = true
		return ErrPersistenceUncertain
	}
	return nil
}

// mediaEngineGroup reports a group the registry file gives to the media engine.
// Its requests use a different adapter, so it is reached from the group list.
func (s *StudioService) mediaEngineGroup(id int64) bool {
	for _, c := range s.Capabilities {
		if c.GroupID == id && c.Engine != "sub2api" {
			return true
		}
	}
	return false
}

// studioOwnKey reads one key through Main with the caller's session, so Main
// decides ownership; the result is checked again before it is used.
func (s *Server) studioOwnKey(r *http.Request, id int64) (studioKey, error) {
	user := r.Context().Value(identityKey{}).(Identity)
	var key studioKey
	if e := s.mainAPI(r, "GET", "/api/v1/keys/"+strconv.FormatInt(id, 10), nil, "", &key); e != nil {
		return studioKey{}, e
	}
	if key.ID != id || key.UserID != user.ID || key.GroupID < 1 || key.Key == "" || key.Status != "active" || key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now()) || s.Studio.mediaEngineGroup(key.GroupID) {
		return studioKey{}, ErrCredential
	}
	return key, nil
}

// studioCustomConnection returns the resolved origin and the client base path.
func (s *Server) studioCustomConnection(r *http.Request, canvas bool) (studioKey, StudioCapability, string, string, map[string]any, error) {
	user := r.Context().Value(identityKey{}).(Identity)
	binding, ok := s.Studio.customBinding(user.ID)
	if !ok || !s.Studio.customAllowed() {
		return studioKey{}, StudioCapability{}, "", "", nil, errStudioCustomNone
	}
	key, e := s.studioOwnKey(r, binding.KeyID)
	if e != nil {
		return studioKey{}, StudioCapability{}, "", "", nil, e
	}
	capability := StudioCapability{GroupID: studioCustomGroupID, Engine: "sub2api", Models: binding.Models, DisplayName: "自定义连接", Protocol: binding.Protocol}
	basePath, task := studioCustomTask(binding, canvas)
	return key, capability, s.Studio.customAPIOrigin(binding), basePath, task, nil
}

type studioCustomKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Hint      string `json:"hint"`
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name"`
	Platform  string `json:"platform"`
	// Protocol and Prefix are where Main serves images for the key's platform;
	// the dialog may change the protocol; the prefix remains derived.
	Protocol string `json:"protocol"`
	Prefix   string `json:"prefix"`
	// Blocker is why this key cannot be used here; empty when it can.
	Blocker string `json:"blocker,omitempty"`
}

func (s *Server) studioCustomKeys(r *http.Request) ([]studioCustomKey, error) {
	user := r.Context().Value(identityKey{}).(Identity)
	var list struct {
		Items []struct {
			studioKey
			Group *struct {
				Name     string `json:"name"`
				Platform string `json:"platform"`
			} `json:"group"`
		} `json:"items"`
	}
	if e := s.mainAPI(r, "GET", "/api/v1/keys?page=1&page_size=100", nil, "", &list); e != nil {
		return nil, e
	}
	out := make([]studioCustomKey, 0, len(list.Items))
	for _, item := range list.Items {
		if item.ID < 1 || item.UserID != user.ID || item.Status != "active" || item.ExpiresAt != nil && !item.ExpiresAt.After(time.Now()) {
			continue
		}
		key := studioCustomKey{ID: item.ID, Name: item.Name, GroupID: item.GroupID}
		if n := len(item.Key); n >= 12 {
			key.Hint = item.Key[n-4:]
		}
		if item.Group != nil {
			key.GroupName, key.Platform = item.Group.Name, item.Group.Platform
		}
		key.Protocol, key.Prefix = studioCustomDefaults(key.Platform)
		// No platform is refused here: this is the way in for a group the
		// registry cannot describe, and Main decides what the key may call.
		switch {
		case item.GroupID < 1:
			key.Blocker = "no_group"
		case s.Studio.mediaEngineGroup(item.GroupID):
			key.Blocker = "media_engine_group"
		}
		out = append(out, key)
	}
	return out, nil
}

func (s *Server) studioCustomInfo(user Identity) map[string]any {
	_, configured := s.Studio.customBinding(user.ID)
	enabled := s.Studio.customAllowed()
	return map[string]any{"group_id": studioCustomGroupID, "setup_id": studioCustomSetupID, "enabled": enabled, "configured": enabled && configured}
}

func (s *Server) studioCustomRoutes(mux *http.ServeMux) {
	unavailable := func(w http.ResponseWriter, e error) {
		status := 503
		if errors.Is(e, ErrCredential) || errors.Is(e, ErrNotFound) {
			status = 403
		}
		jsonResponse(w, status, map[string]string{"error": "custom_connection_unavailable"})
	}
	allowed := func(w http.ResponseWriter) bool {
		if s.Studio.customAllowed() {
			return true
		}
		jsonResponse(w, 403, map[string]string{"error": "custom_connection_disabled"})
		return false
	}
	mux.HandleFunc("GET /api/studio/custom", func(w http.ResponseWriter, r *http.Request) {
		user := r.Context().Value(identityKey{}).(Identity)
		out := s.studioCustomInfo(user)
		out["api_origin"] = s.Studio.APIOrigin
		out["api_host"] = s.Studio.customAPIHost()
		out["api_labels"], out["api_default"], _ = s.Studio.customAPISettings()
		if out["enabled"] == true {
			keys, e := s.studioCustomKeys(r)
			if e != nil {
				unavailable(w, e)
				return
			}
			out["keys"] = keys
			if binding, ok := s.Studio.customBinding(user.ID); ok {
				out["binding"] = binding
			}
		}
		jsonResponse(w, 200, out)
	})
	mux.HandleFunc("POST /api/studio/custom/models", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			KeyID    int64           `json:"key_id"`
			Protocol string          `json:"protocol"`
			Prefix   json.RawMessage `json:"prefix"` // Ignored for older callers.
		}
		if decode(w, r, &in) != nil || in.KeyID < 1 {
			jsonResponse(w, 400, map[string]string{"error": ErrStudioCustom.Error()})
			return
		}
		if !allowed(w) {
			return
		}
		key, e := s.studioOwnKey(r, in.KeyID)
		if e != nil {
			unavailable(w, e)
			return
		}
		prefix, e := studioCustomPathPrefix(studioKeyPlatform(key), in.Protocol)
		if e != nil {
			jsonResponse(w, 400, map[string]string{"error": ErrStudioCustom.Error()})
			return
		}
		models, e := s.gatewayModels(r, key.Key, prefix)
		if e != nil {
			unavailable(w, e)
			return
		}
		jsonResponse(w, 200, map[string]any{"models": models})
	})
	mux.HandleFunc("PUT /api/studio/custom", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			KeyID    int64           `json:"key_id"`
			Protocol string          `json:"protocol"`
			Prefix   json.RawMessage `json:"prefix"` // Ignored for older callers.
			APILabel string          `json:"api_label"`
			Models   []string        `json:"models"`
		}
		if decode(w, r, &in) != nil || in.KeyID < 1 {
			jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
			return
		}
		if !allowed(w) {
			return
		}
		key, e := s.studioOwnKey(r, in.KeyID)
		if e != nil {
			unavailable(w, e)
			return
		}
		prefix, e := studioCustomPathPrefix(studioKeyPlatform(key), in.Protocol)
		labels, _, _ := s.Studio.customAPISettings()
		if studioKeyPlatform(key) == "antigravity" {
			in.APILabel = ""
		}
		labelOK := slices.Contains(labels, in.APILabel) || in.APILabel == "" && (len(labels) == 0 || studioKeyPlatform(key) == "antigravity")
		binding, invalid := studioCustomBinding(StudioCustomBinding{KeyID: in.KeyID, Protocol: in.Protocol, Prefix: prefix, Models: in.Models, APILabel: in.APILabel})
		if e != nil || invalid != nil || !labelOK {
			jsonResponse(w, 400, map[string]string{"error": ErrStudioCustom.Error()})
			return
		}
		binding.UpdatedAt = time.Now().UTC()
		user := r.Context().Value(identityKey{}).(Identity)
		if e = s.Studio.saveCustom(user.ID, &binding); e != nil {
			jsonResponse(w, 500, map[string]string{"error": "storage_unavailable"})
			return
		}
		jsonResponse(w, 200, s.studioCustomInfo(user))
	})
	mux.HandleFunc("DELETE /api/studio/custom", func(w http.ResponseWriter, r *http.Request) {
		user := r.Context().Value(identityKey{}).(Identity)
		if e := s.Studio.saveCustom(user.ID, nil); e != nil {
			jsonResponse(w, 500, map[string]string{"error": "storage_unavailable"})
			return
		}
		w.WriteHeader(204)
	})
}

var studioAPILabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func firstStudioAPILabel(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return labels[0]
}

func validateStudioAPILabels(labels []string, defaultLabel string) error {
	if len(labels) > 16 || len(labels) == 0 && defaultLabel != "" || len(labels) > 0 && !slices.Contains(labels, defaultLabel) {
		return ErrStudioRegistry
	}
	seen := map[string]bool{}
	for _, label := range labels {
		if !studioAPILabel.MatchString(label) || seen[label] {
			return ErrStudioRegistry
		}
		seen[label] = true
	}
	return nil
}

// ParseStudioCustomAPILabels validates deployment defaults without echoing input.
func ParseStudioCustomAPILabels(raw string) ([]string, error) {
	labels := []string{}
	if raw != "" {
		for _, label := range strings.Split(raw, ",") {
			labels = append(labels, strings.TrimSpace(label))
		}
	}
	if e := validateStudioAPILabels(labels, firstStudioAPILabel(labels)); e != nil {
		return nil, e
	}
	return labels, nil
}

// customAPISettings returns a detached snapshot; online settings override deployment defaults.
func (s *StudioService) customAPISettings() ([]string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.registry.CustomAPILabels) > 0 {
		return append([]string{}, s.registry.CustomAPILabels...), s.registry.CustomAPIDefault, "online"
	}
	if len(s.deploymentLabels) > 0 {
		return append([]string{}, s.deploymentLabels...), firstStudioAPILabel(s.deploymentLabels), "deployment"
	}
	return []string{}, "", "none"
}

func (s *StudioService) customAPIHost() string {
	u, _ := url.Parse(s.APIOrigin)
	return u.Hostname()
}

func (s *StudioService) customAPIOrigin(b StudioCustomBinding) string {
	if strings.HasPrefix(b.Prefix, "/antigravity/") || b.APILabel == "" {
		return s.APIOrigin
	}
	labels, defaultLabel, _ := s.customAPISettings()
	label := b.APILabel
	if !slices.Contains(labels, label) {
		label = defaultLabel
	}
	if label == "" {
		return s.APIOrigin
	}
	return "https://" + label + "." + s.customAPIHost()
}

func studioKeyPlatform(key studioKey) string {
	if key.Group == nil {
		return ""
	}
	return key.Group.Platform
}

func studioCustomPathPrefix(platform, protocol string) (string, error) {
	if platform == "antigravity" {
		if protocol == "gemini_native" {
			return "/antigravity/v1beta", nil
		}
		return "", ErrStudioCustom
	}
	switch protocol {
	case "gemini_native":
		return "/v1beta", nil
	case "openai_images", "grok_image":
		return "/v1", nil
	}
	return "", ErrStudioCustom
}
