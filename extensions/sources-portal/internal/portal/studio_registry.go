package portal

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Group IDs from this value up never name a Main group. The Studio and Canvas
// clients accept only connections called group-<positive integer>, so entries
// that are not Main groups use reserved IDs instead of a second naming scheme.
const (
	studioReservedGroupID int64 = 9_000_000_000_000_000
	// studioCustomGroupID is the user's own-key connection.
	studioCustomGroupID = studioReservedGroupID + 1
	// The next two are menu entries of the Portal page. They are never
	// connected to: choosing one opens a settings dialog.
	studioCustomSetupID = studioReservedGroupID + 2
	studioManageID      = studioReservedGroupID + 3
)

var studioModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

var (
	ErrStudioRegistry      = errors.New("invalid_studio_registry")
	ErrStudioServerManaged = errors.New("studio_group_is_server_managed")
)

// StudioManagedGroup is an ordinary group an administrator added or changed in
// the browser. The engine is never taken from a request: a managed group is
// always a native one. Media engine groups stay in the operator's registry
// file, because Main reads the same file to pick accounts and check models.
type StudioManagedGroup struct {
	GroupID int64    `json:"group_id"`
	Models  []string `json:"models"`
	Enabled bool     `json:"enabled"`
}

type studioRegistryState struct {
	Revision         int64                `json:"revision"`
	UpdatedAt        time.Time            `json:"updated_at"`
	UpdatedBy        int64                `json:"updated_by"`
	CustomAPILabels  []string             `json:"custom_api_labels,omitempty"`
	CustomAPIDefault string               `json:"custom_api_default,omitempty"`
	CustomDisabled   bool                 `json:"custom_disabled"`
	Groups           []StudioManagedGroup `json:"groups"`
}

// studioModels trims and de-duplicates a model list; ok is false when a model
// ID is malformed or the list is empty or longer than a capability may hold.
func studioModels(in []string) ([]string, bool) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, model := range in {
		model = strings.TrimSpace(model)
		if !studioModelID.MatchString(model) {
			return nil, false
		}
		if !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out, len(out) > 0 && len(out) <= 32
}

func studioManagedGroups(in []StudioManagedGroup) ([]StudioManagedGroup, error) {
	if len(in) > 200 {
		return nil, ErrStudioRegistry
	}
	out := make([]StudioManagedGroup, 0, len(in))
	seen := map[int64]bool{}
	for _, group := range in {
		models, ok := studioModels(group.Models)
		if group.GroupID < 1 || group.GroupID >= studioReservedGroupID || seen[group.GroupID] || !ok {
			return nil, ErrStudioRegistry
		}
		seen[group.GroupID] = true
		out = append(out, StudioManagedGroup{GroupID: group.GroupID, Models: models, Enabled: group.Enabled})
	}
	return out, nil
}

// mergeStudioCapabilities applies the managed groups on top of the registry
// file. A managed entry replaces a native file entry of the same group, and a
// disabled one hides it. A media engine entry is never replaced: if the file
// later claims a managed group for the media engine, the file wins.
func mergeStudioCapabilities(file []StudioCapability, managed []StudioManagedGroup) []StudioCapability {
	byID := make(map[int64]StudioManagedGroup, len(managed))
	for _, group := range managed {
		byID[group.GroupID] = group
	}
	out := make([]StudioCapability, 0, len(file)+len(managed))
	inFile := make(map[int64]bool, len(file))
	for _, c := range file {
		inFile[c.GroupID] = true
		group, ok := byID[c.GroupID]
		if !ok || c.Engine != "sub2api" {
			out = append(out, c)
		} else if group.Enabled {
			out = append(out, StudioCapability{GroupID: c.GroupID, Engine: "sub2api", Models: group.Models})
		}
	}
	for _, group := range managed {
		if !inFile[group.GroupID] && group.Enabled {
			out = append(out, StudioCapability{GroupID: group.GroupID, Engine: "sub2api", Models: group.Models})
		}
	}
	return out
}

func (s *StudioService) loadRegistry() error {
	b, e := os.ReadFile(s.registryPath)
	if os.IsNotExist(e) {
		s.effective = mergeStudioCapabilities(s.Capabilities, nil)
		return nil
	}
	if e != nil {
		return e
	}
	var state studioRegistryState
	if json.Unmarshal(b, &state) != nil || state.Revision < 1 {
		return ErrStudioRegistry
	}
	if state.Groups, e = studioManagedGroups(state.Groups); e != nil {
		return e
	}
	if e = validateStudioAPILabels(state.CustomAPILabels, state.CustomAPIDefault); e != nil {
		return e
	}
	s.registry = state
	s.effective = mergeStudioCapabilities(s.Capabilities, state.Groups)
	return nil
}

// capabilities is the list users are served from: the registry file with the
// managed groups applied. The slice is replaced on save, never changed in place.
func (s *StudioService) capabilities() []StudioCapability {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective
}

func (s *StudioService) registrySnapshot() (studioRegistryState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registry, !s.registryFailed
}

func (s *StudioService) customAllowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.registry.CustomDisabled
}

// saveRegistry replaces the managed groups if revision is still the current
// one. The change takes effect for the next request; nothing restarts.
func (s *StudioService) saveRegistry(revision int64, customDisabled bool, groups []StudioManagedGroup, admin int64, labels []string, defaultLabel string) error {
	if e := validateStudioAPILabels(labels, defaultLabel); e != nil {
		return e
	}
	labels = append([]string(nil), labels...)
	groups, e := studioManagedGroups(groups)
	if e != nil {
		return e
	}
	for _, group := range groups {
		for _, c := range s.Capabilities {
			if c.GroupID == group.GroupID && c.Engine != "sub2api" {
				return ErrStudioServerManaged
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryFailed {
		return ErrPersistenceUncertain
	}
	if revision != s.registry.Revision {
		return ErrConflict
	}
	next := studioRegistryState{Revision: revision + 1, UpdatedAt: time.Now().UTC(), UpdatedBy: admin, CustomDisabled: customDisabled, Groups: groups, CustomAPILabels: labels, CustomAPIDefault: defaultLabel}
	e = writeJSONAtomic(s.registryPath, next)
	if e != nil && !writeCommitted(e) {
		return e
	}
	s.registry = next
	s.effective = mergeStudioCapabilities(s.Capabilities, groups)
	if e != nil {
		// The file was replaced but the directory sync failed. Memory matches the
		// file; further edits wait for an operator to check and restart.
		s.registryFailed = true
		return ErrPersistenceUncertain
	}
	return nil
}

type studioMainGroup struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`
	AllowImages bool   `json:"allow_image_generation"`
}

// studioRegistryRow is one group as the settings dialog shows it.
type studioRegistryRow struct {
	GroupID  int64  `json:"group_id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	// Source is "file" (registry file only), "managed" (added in the browser),
	// "override" (file entry changed in the browser) or "none".
	Source string `json:"source"`
	// Locked marks a media engine group: it is configured in the file only.
	Locked  bool     `json:"locked"`
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
	// FileModels is what removing an override goes back to.
	FileModels []string `json:"file_models,omitempty"`
	// Blockers says why users do not see a registered group yet.
	Blockers []string `json:"blockers"`
}

func studioNativePlatform(platform string) bool {
	return platform == "openai" || platform == "grok" || platform == "gemini"
}

func (s *Server) studioRegistryView(r *http.Request) (map[string]any, error) {
	var groups []studioMainGroup
	if e := s.mainAPI(r, "GET", mainGroupListPath, nil, "", &groups); e != nil {
		return nil, e
	}
	state, writable := s.Studio.registrySnapshot()
	managed := make(map[int64]StudioManagedGroup, len(state.Groups))
	for _, group := range state.Groups {
		managed[group.GroupID] = group
	}
	file := make(map[int64]StudioCapability, len(s.Studio.Capabilities))
	for _, c := range s.Studio.Capabilities {
		file[c.GroupID] = c
	}
	row := func(id int64) studioRegistryRow {
		out := studioRegistryRow{GroupID: id, Source: "none", Models: []string{}, Blockers: []string{}}
		c, inFile := file[id]
		group, isManaged := managed[id]
		if inFile {
			out.Source, out.Enabled, out.Models, out.Locked = "file", true, c.Models, c.Engine != "sub2api"
		}
		if isManaged && !out.Locked {
			out.Source, out.Enabled, out.Models = "managed", group.Enabled, group.Models
			if inFile {
				out.Source, out.FileModels = "override", c.Models
			}
		}
		return out
	}
	rows := make([]studioRegistryRow, 0, len(groups)+len(state.Groups))
	listed := make(map[int64]bool, len(groups))
	for _, g := range groups {
		if g.ID < 1 || listed[g.ID] {
			continue
		}
		listed[g.ID] = true
		out := row(g.ID)
		out.Name, out.Platform = g.Name, g.Platform
		if g.Status != "active" {
			out.Blockers = append(out.Blockers, "group_inactive")
		}
		if !g.AllowImages {
			out.Blockers = append(out.Blockers, "image_generation_disabled")
		}
		if !studioNativePlatform(g.Platform) || out.Locked && g.Platform == "grok" {
			out.Blockers = append(out.Blockers, "unsupported_platform")
		}
		rows = append(rows, out)
	}
	// A registered group Main no longer lists is shown so it can be removed.
	missing := func(id int64) {
		if !listed[id] {
			listed[id] = true
			out := row(id)
			out.Blockers = []string{"group_missing"}
			rows = append(rows, out)
		}
	}
	for _, c := range s.Studio.Capabilities {
		missing(c.GroupID)
	}
	for _, group := range state.Groups {
		missing(group.GroupID)
	}
	_, _, source := s.Studio.customAPISettings()
	return map[string]any{
		"revision": state.Revision, "writable": writable, "groups": rows,
		"custom_disabled": state.CustomDisabled, "api_host": s.Studio.customAPIHost(),
		"custom_api_labels": state.CustomAPILabels, "custom_api_default": state.CustomAPIDefault,
		"deployment_api_labels":  s.Studio.deploymentLabels,
		"deployment_api_default": firstStudioAPILabel(s.Studio.deploymentLabels),
		"custom_api_source":      source,
	}, nil
}

func studioAdmin(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	user := r.Context().Value(identityKey{}).(Identity)
	if user.Role != "admin" {
		jsonResponse(w, 403, map[string]string{"error": "admin_required"})
		return user, false
	}
	return user, true
}

// The /api/studio/ prefix is open to every signed-in user, so each of these
// routes checks the administrator role itself.
func (s *Server) studioRegistryRoutes(mux *http.ServeMux) {
	view := func(w http.ResponseWriter, r *http.Request) {
		out, e := s.studioRegistryView(r)
		if e != nil {
			status := 503
			if errors.Is(e, ErrCredential) {
				status = 403
			}
			jsonResponse(w, status, map[string]string{"error": "studio_registry_unavailable"})
			return
		}
		jsonResponse(w, 200, out)
	}
	mux.HandleFunc("GET /api/studio/admin/registry", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := studioAdmin(w, r); ok {
			view(w, r)
		}
	})
	mux.HandleFunc("PUT /api/studio/admin/registry", func(w http.ResponseWriter, r *http.Request) {
		admin, ok := studioAdmin(w, r)
		if !ok {
			return
		}
		var in struct {
			Revision         int64                `json:"revision"`
			CustomAPILabels  []string             `json:"custom_api_labels,omitempty"`
			CustomAPIDefault string               `json:"custom_api_default,omitempty"`
			CustomDisabled   bool                 `json:"custom_disabled"`
			Groups           []StudioManagedGroup `json:"groups"`
		}
		if decode(w, r, &in) != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
			return
		}
		switch e := s.Studio.saveRegistry(in.Revision, in.CustomDisabled, in.Groups, admin.ID, in.CustomAPILabels, in.CustomAPIDefault); {
		case e == nil:
			view(w, r)
		case errors.Is(e, ErrStudioRegistry):
			jsonResponse(w, 400, map[string]string{"error": ErrStudioRegistry.Error()})
		case errors.Is(e, ErrStudioServerManaged), errors.Is(e, ErrConflict):
			jsonResponse(w, 409, map[string]string{"error": e.Error()})
		case errors.Is(e, ErrPersistenceUncertain):
			jsonResponse(w, 500, map[string]string{"error": ErrPersistenceUncertain.Error()})
		default:
			jsonResponse(w, 500, map[string]string{"error": "storage_unavailable"})
		}
	})
	mux.HandleFunc("GET /api/studio/admin/groups/{id}/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := studioAdmin(w, r); !ok {
			return
		}
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 || id >= studioReservedGroupID {
			jsonResponse(w, 400, map[string]string{"error": "invalid_group"})
			return
		}
		var reply struct {
			Models []string `json:"models"`
		}
		if e = s.mainAPI(r, "GET", "/api/v1/admin/groups/"+strconv.FormatInt(id, 10)+"/model-allowlist-candidates", nil, "", &reply); e != nil {
			status := 503
			if errors.Is(e, ErrNotFound) {
				status = 404
			}
			jsonResponse(w, status, map[string]string{"error": "studio_models_unavailable"})
			return
		}
		models := make([]string, 0, len(reply.Models))
		for _, model := range reply.Models {
			if len(models) < 1000 && studioModelID.MatchString(model) {
				models = append(models, model)
			}
		}
		jsonResponse(w, 200, map[string]any{"models": models})
	})
}
