package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type StudioGroup struct {
	IsExclusive bool   `json:"is_exclusive"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`
	AllowImages bool   `json:"allow_image_generation"`
}
type StudioCapability struct {
	DisplayName string   `json:"-"`
	Protocol    string   `json:"-"`
	GroupID     int64    `json:"group_id"`
	Models      []string `json:"models"`
	Engine      string   `json:"engine"`
}
type StudioBinding struct {
	Attempted bool      `json:"attempted"`
	UserID    int64     `json:"user_id"`
	GroupID   int64     `json:"group_id"`
	KeyID     int64     `json:"key_id"`
	Operation string    `json:"operation"`
	CreatedAt time.Time `json:"created_at"`
}
type StudioService struct {
	mu       sync.Mutex
	locks    map[string]chan struct{}
	failed   error
	path     string
	bindings map[string]StudioBinding
	// Capabilities is the operator's registry file and does not change while
	// the process runs. Users are served from capabilities(), which applies the
	// groups an administrator manages in the browser on top of it.
	Capabilities   []StudioCapability
	effective      []StudioCapability
	registry       studioRegistryState
	registryPath   string
	registryFailed bool
	custom         map[string]StudioCustomBinding
	customPath     string
	customFailed   bool
	APIOrigin      string
	// mediaEngine is the canonical engine value of media engine capabilities
	// and the value the Studio client receives.
	mediaEngine string
}

// studioMediaEngineNeutral is the media engine value of a fresh install. Main
// reads the same registry and accepts the same values.
const studioMediaEngineNeutral = "media"

// studioMediaEngineLegacyName matches Main's check of the same setting.
var studioMediaEngineLegacyName = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

// studioMediaEngineSelector returns the engine value to emit. A deployment
// whose registry and Studio client already use an earlier value configures it
// as the legacy name, so the client keeps receiving exactly that value.
func studioMediaEngineSelector(legacyName string) (string, error) {
	if legacyName == "" {
		return studioMediaEngineNeutral, nil
	}
	// "sub2api" is the registry's other engine value and cannot name this one.
	if !studioMediaEngineLegacyName.MatchString(legacyName) || legacyName == "sub2api" {
		return "", errors.New("invalid media engine legacy name")
	}
	return legacyName, nil
}

type studioKey struct {
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	GroupID   int64      `json:"group_id"`
	Key       string     `json:"key"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// OpenStudio accepts either the neutral or the legacy media engine value in
// capabilities; mediaEngineLegacyName is "" on a fresh install.
func OpenStudio(dir, origin string, capabilities []StudioCapability, mediaEngineLegacyName string) (*StudioService, error) {
	mediaEngine, err := studioMediaEngineSelector(mediaEngineLegacyName)
	if err != nil {
		return nil, err
	}
	// Copy before canonicalizing so the caller's configuration is not rewritten.
	capabilities = append([]StudioCapability(nil), capabilities...)
	s := &StudioService{path: filepath.Join(dir, "studio-bindings.json"), registryPath: filepath.Join(dir, "studio-registry.json"), customPath: filepath.Join(dir, "studio-custom.json"), bindings: map[string]StudioBinding{}, custom: map[string]StudioCustomBinding{}, locks: map[string]chan struct{}{}, APIOrigin: origin, Capabilities: capabilities, mediaEngine: mediaEngine}
	seen := map[int64]bool{}
	for i, c := range capabilities {
		if c.Engine == studioMediaEngineNeutral || c.Engine == mediaEngine {
			capabilities[i].Engine = mediaEngine
		} else if c.Engine != "sub2api" {
			return nil, errors.New("invalid_studio_config_or_engine_not_integrated")
		}
		if c.GroupID < 1 || c.GroupID >= studioReservedGroupID || seen[c.GroupID] || len(c.Models) == 0 || len(c.Models) > 32 {
			return nil, errors.New("invalid_studio_config_or_engine_not_integrated")
		}
		seen[c.GroupID] = true
	}
	b, e := os.ReadFile(s.path)
	if e == nil {
		if json.Unmarshal(b, &s.bindings) != nil || s.bindings == nil {
			return nil, errors.New("invalid_studio_bindings")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if e = s.loadRegistry(); e != nil {
		return nil, errors.New("invalid_studio_registry")
	}
	if e = s.loadCustom(); e != nil {
		return nil, errors.New("invalid_studio_custom_connections")
	}
	return s, nil
}
func (s *Server) studioGroups(r *http.Request) ([]StudioGroup, error) {
	var groups []StudioGroup
	if e := s.mainAPI(r, "GET", "/api/v1/groups/available", nil, "", &groups); e != nil {
		return nil, e
	}
	out := []StudioGroup{}
	capabilities := s.Studio.capabilities()
	for _, g := range groups {
		for _, c := range capabilities {
			if g.ID == c.GroupID && g.Status == "active" && g.AllowImages && ((c.Engine == "sub2api" && (g.Platform == "openai" || g.Platform == "grok" || g.Platform == "gemini")) || (c.Engine == s.Studio.mediaEngine && (g.Platform == "openai" || g.Platform == "gemini"))) {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}
func (s *Server) studioConnection(r *http.Request, groupID int64) (studioKey, StudioCapability, error) {
	user := r.Context().Value(identityKey{}).(Identity)
	groups, e := s.studioGroups(r)
	if e != nil {
		return studioKey{}, StudioCapability{}, e
	}
	allowed := false
	groupName := ""
	protocol := "openai_images"
	platform := ""
	for _, g := range groups {
		if g.ID == groupID {
			allowed = true
			groupName = g.Name
			platform = g.Platform
			if g.Platform == "grok" {
				protocol = "grok_image"
			}
		}
	}
	if !allowed {
		return studioKey{}, StudioCapability{}, ErrCredential
	}
	var capability StudioCapability
	for _, c := range s.Studio.capabilities() {
		if c.GroupID == groupID {
			capability = c
			capability.DisplayName = groupName
			capability.Protocol = protocol
			// 媒体引擎的Gemini 仍使用原持久任务；普通 Gemini 走原生接口。
			if platform == "gemini" && c.Engine == "sub2api" {
				capability.Protocol = "gemini_native"
			}
		}
	}
	st := s.Studio
	index := fmt.Sprintf("%d:%d", user.ID, groupID)
	unlock, e := st.lock(r.Context(), index)
	if e != nil {
		return studioKey{}, capability, e
	}
	defer unlock()
	st.mu.Lock()
	binding, exists := st.bindings[index]
	failed := st.failed
	st.mu.Unlock()
	if failed != nil {
		return studioKey{}, capability, ErrPersistenceUncertain
	}
	if !exists {
		binding = StudioBinding{UserID: user.ID, GroupID: groupID, Operation: randomID(), CreatedAt: time.Now().UTC()}
		if e = st.save(index, binding); e != nil {
			return studioKey{}, capability, e
		}
	}
	var key studioKey
	if binding.KeyID > 0 {
		e = s.mainAPI(r, "GET", "/api/v1/keys/"+strconv.FormatInt(binding.KeyID, 10), nil, "", &key)
	} else {
		// Reconcile an ambiguous create before retrying its idempotent operation. Never
		// make a new operation ID just because the HTTP response was lost.
		name := "图片工作台 · " + binding.Operation
		var list struct {
			Items []studioKey `json:"items"`
		}
		e = s.mainAPI(r, "GET", "/api/v1/keys?search="+binding.Operation+"&page_size=100", nil, "", &list)
		if e != nil {
			return key, capability, e
		}
		for _, candidate := range list.Items {
			if candidate.Name == name && candidate.GroupID == groupID {
				if key.ID != 0 {
					return key, capability, ErrConflict
				}
				key = candidate
			}
		}
		if key.ID == 0 {
			if binding.Attempted {
				return key, capability, ErrConflict
			}
			binding.Attempted = true
			if e = st.save(index, binding); e != nil {
				return key, capability, e
			}
			e = s.mainAPI(r, "POST", "/api/v1/keys", map[string]any{"name": name, "group_id": groupID, "expires_in_days": 30}, "studio-"+binding.Operation, &key)
			if e != nil {
				return key, capability, e
			}
		}
		if key.ID > 0 && key.UserID == user.ID && key.GroupID == groupID {
			binding.KeyID = key.ID
			if e = st.save(index, binding); e != nil {
				return studioKey{}, capability, e
			}
		}
	}
	if e != nil {
		return studioKey{}, capability, e
	}
	if key.ID == 0 || key.UserID != user.ID || key.GroupID != groupID || key.Key == "" || key.Status != "active" || key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now()) {
		return studioKey{}, capability, ErrCredential
	}
	// The operation name is needed until KeyID has been durably saved: a lost
	// create response must remain reconcilable. Native conditional rename also
	// protects a concurrent user rename, which this Portal-only lock cannot do.
	if key.Name == "图片工作台 · "+binding.Operation {
		var renamed struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		name := studioKeyDisplayName(groupName)
		err := s.mainAPI(r, http.MethodPatch, "/api/v1/keys/"+strconv.FormatInt(key.ID, 10)+"/name", map[string]any{"name": name, "expected_name": key.Name, "expected_group_id": groupID}, "", &renamed)
		if err == nil && renamed.ID == key.ID && renamed.Name == html.EscapeString(name) {
			key.Name = renamed.Name
		}
		// Unsupported old Main, conflicts, or unknown outcomes never fall back
		// to unconditional PUT. A later connection re-reads the same saved KeyID.
	}
	return key, capability, nil
}

func studioKeyDisplayName(groupName string) string {
	name := "生图 · "
	for _, r := range strings.TrimSpace(groupName) {
		next := name + string(r)
		if len(html.EscapeString(next)) > 100 {
			break
		}
		name = next
	}
	return strings.TrimSpace(name)
}

func studioViewer(user Identity) map[string]any {
	name := strings.TrimSpace(user.Username)
	return map[string]any{"displayName": name, "balance": user.Balance}
}
func (s *Server) studioRoutes(mux *http.ServeMux) {
	if s.Studio == nil {
		return
	}
	mux.HandleFunc("GET /api/studio/groups", func(w http.ResponseWriter, r *http.Request) {
		groups, e := s.studioGroups(r)
		if e != nil {
			apiError(w, e)
			return
		}
		user := r.Context().Value(identityKey{}).(Identity)
		jsonResponse(w, 200, map[string]any{"user_id": user.ID, "groups": groups, "studio_url": s.StudioURL, "main_origin": s.SubOrigin, "viewer": studioViewer(user), "custom": s.studioCustomInfo(user), "can_manage": user.Role == "admin", "manage_id": studioManageID})
	})
	connection := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			GroupID     int64 `json:"group_id"`
			RestoreOnly bool  `json:"restore_only"`
		}
		if decode(w, r, &in) != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
			return
		}
		var key studioKey
		var c StudioCapability
		var task map[string]any
		var e error
		// basePath is set only by a custom connection under a prefix the Studio
		// client must be given as part of its base address.
		basePath := ""
		if in.GroupID == studioCustomGroupID {
			// The user's own key: nothing is created here, so a restore and a
			// first connection read the same saved binding.
			key, c, basePath, task, e = s.studioCustomConnection(r, r.URL.Path == "/api/canvas/connection")
			if errors.Is(e, errStudioCustomNone) {
				jsonResponse(w, 403, map[string]string{"error": "existing_authorization_required"})
				return
			}
		} else {
			if in.RestoreOnly {
				user := r.Context().Value(identityKey{}).(Identity)
				s.Studio.mu.Lock()
				binding := s.Studio.bindings[fmt.Sprintf("%d:%d", user.ID, in.GroupID)]
				s.Studio.mu.Unlock()
				if binding.KeyID == 0 {
					jsonResponse(w, 403, map[string]string{"error": "existing_authorization_required"})
					return
				}
			}
			key, c, e = s.studioConnection(r, in.GroupID)
			task = studioTask(c.Engine, c.Protocol)
		}
		if e != nil {
			status := 503
			if errors.Is(e, ErrCredential) || errors.Is(e, ErrNotFound) {
				status = 403
			}
			if errors.Is(e, ErrConflict) {
				status = 409
			}
			jsonResponse(w, status, map[string]string{"error": "studio_authorization_unavailable", "message": "授权已撤销、过期或创建结果待核对；请查看主站 API 密钥。不会自动重建或换组。"})
			return
		}
		id := fmt.Sprintf("group-%d", in.GroupID)
		models := []map[string]any{}
		for i, m := range c.Models {
			models = append(models, map[string]any{"id": m, "label": m, "isDefault": i == 0, "parameters": studioModelParameters(c.Engine == s.Studio.mediaEngine, c.Protocol, m)})
		}
		transport := map[string]any{"baseUrl": s.Studio.APIOrigin + basePath, "credentialRef": key.Key}
		if key.ExpiresAt != nil {
			transport["expiresAt"] = key.ExpiresAt.Unix()
		}
		payload := map[string]any{"version": 2, "surface": "image_studio", "retainGrantedConnections": true, "connections": []any{map[string]any{"id": id, "protocol": c.Protocol, "transport": transport}}, "capabilities": []any{map[string]any{"id": id, "displayName": c.DisplayName, "connectionId": id, "protocol": c.Protocol, "mediaType": "image", "inputModes": []string{"text", "image"}, "models": models, "task": task}}, "defaults": map[string]string{"image": id + ":" + c.Models[0]}, "limits": map[string]int{"maxParallelRequests": 2, "timeoutSeconds": 1800}}
		payload["selectedConnectionId"] = id
		payload["selectActive"] = !in.RestoreOnly
		payload["authorizationName"] = key.Name
		if r.URL.Path == "/api/canvas/connection" {
			payload["surface"] = "media"
			payload["user_id"] = r.Context().Value(identityKey{}).(Identity).ID
			payload["main_origin"] = s.SubOrigin
			payload["selectedConnectionId"] = id
			payload["selectActive"] = !in.RestoreOnly
		}
		jsonResponse(w, 200, payload)
	}
	mux.HandleFunc("POST /api/studio/connection", connection)
	s.studioRegistryRoutes(mux)
	s.studioCustomRoutes(mux)
	if s.CanvasURL != "" {
		mux.HandleFunc("POST /api/canvas/connection", connection)
		mux.HandleFunc("GET /api/canvas/groups", func(w http.ResponseWriter, r *http.Request) {
			groups, e := s.studioGroups(r)
			if e != nil {
				apiError(w, e)
				return
			}
			user := r.Context().Value(identityKey{}).(Identity)
			jsonResponse(w, 200, map[string]any{"user_id": user.ID, "groups": groups, "canvas_url": s.CanvasURL, "main_origin": s.SubOrigin, "custom": s.studioCustomInfo(user), "can_manage": user.Role == "admin"})
		})
		mux.HandleFunc("GET /canvas", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, webFiles, "web/canvas.html") })
	}
	mux.HandleFunc("GET /studio", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, webFiles, "web/studio.html") })
}

func (s *StudioService) lock(ctx context.Context, index string) (func(), error) {
	s.mu.Lock()
	ch, ok := s.locks[index]
	if !ok {
		ch = make(chan struct{}, 1)
		s.locks[index] = ch
	}
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *StudioService) save(index string, binding StudioBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return ErrPersistenceUncertain
	}
	next := make(map[string]StudioBinding, len(s.bindings)+1)
	for k, v := range s.bindings {
		next[k] = v
	}
	next[index] = binding
	e := writeJSONAtomic(s.path, next)
	if e == nil || writeCommitted(e) {
		s.bindings = next
	}
	if e != nil {
		s.failed = e
	}
	return e
}

// studioTask is the request contract of a registered group: native Gemini
// answers in the same request, every other protocol submits a task and polls.
func studioTask(engine, protocol string) map[string]any {
	if protocol == "gemini_native" {
		// 原生 Gemini 同步返回 inlineData，不宣称存在异步任务查询接口。
		return map[string]any{"engine": engine, "modes": []string{"sync"}, "createPath": "/v1beta/models/{model}:generateContent", "editPath": "/v1beta/models/{model}:generateContent", "resultKind": "image"}
	}
	return map[string]any{"engine": engine, "modes": []string{"async"}, "createPath": "/v1/images/generations/async", "editPath": "/v1/images/edits/async", "statusPathTemplate": "/v1/images/tasks/{task_id}", "resultKind": "image"}
}

// These declarations describe the adapter contract, not inferred capabilities
// from a model's name. Main validates the same allowlist before staging inputs.
func studioModelParameters(mediaEngine bool, protocol, model string) []any {
	if protocol == "gemini_native" {
		return []any{
			map[string]any{"key": "size", "type": "string", "supported": true},
			map[string]any{"key": "n", "type": "integer", "supported": true, "min": 1},
			map[string]any{"key": "reference_images", "type": "array", "supported": true},
			map[string]any{"key": "output_format", "type": "enum", "supported": true, "default": "png", "options": []any{map[string]string{"value": "png", "label": "PNG"}}},
			map[string]any{"key": "quality", "type": "string", "supported": false},
			map[string]any{"key": "mask", "type": "image", "supported": false, "reason": "此分组不支持遮罩编辑"},
			map[string]any{"key": "stream", "type": "boolean", "supported": false},
		}
	}
	if protocol == "grok_image" {
		// This is the pinned upstream geometry contract, not the old private
		// managed-images endpoint. The native 202 task path remains unchanged.
		options := func(values ...string) []any {
			out := make([]any, 0, len(values))
			for _, value := range values {
				out = append(out, map[string]string{"value": value, "label": value})
			}
			return out
		}
		return []any{
			map[string]any{"key": "resolution", "type": "enum", "supported": true, "default": "1k", "options": options("1k", "2k")},
			map[string]any{"key": "aspect_ratio", "type": "enum", "supported": true, "default": "1:1", "options": options("1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20")},
			map[string]any{"key": "n", "type": "integer", "supported": true, "min": 1},
		}
	}

	if !mediaEngine {
		return []any{}
	}
	out := []any{
		map[string]any{"key": "size", "type": "string", "supported": true},
		map[string]any{"key": "n", "type": "number", "supported": true, "min": 1, "max": 1},
		map[string]any{"key": "output_format", "type": "string", "supported": true, "options": []any{map[string]string{"value": "png", "label": "PNG"}, map[string]string{"value": "jpeg", "label": "JPEG"}, map[string]string{"value": "webp", "label": "WebP"}}},
		map[string]any{"key": "reference_images", "type": "array", "supported": true, "max": 4},
		map[string]any{"key": "mask", "type": "image", "supported": false, "reason": "此分组不支持遮罩编辑"},
		map[string]any{"key": "stream", "type": "boolean", "supported": false},
	}
	if model == "gpt-image-2" || model == "gpt-image-medium" {
		out = append(out, map[string]any{"key": "quality", "type": "string", "supported": true, "options": []any{map[string]string{"value": "low", "label": "Low"}, map[string]string{"value": "medium", "label": "Medium"}, map[string]string{"value": "high", "label": "High"}}})
	}
	return out
}
