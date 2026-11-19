package portal

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

var hashedPortalAsset = regexp.MustCompile(`^/home-assets/assets/[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8,}\.(js|css)$`)

type Server struct {
	InternalSubOrigin string
	TrustedProxies    []netip.Prefix
	Studio            *StudioService
	StudioURL         string
	CanvasURL         string
	Service           *Service
	SubOrigin         string
	PublicOrigin      string
	AuthClient        *http.Client
	Content           *ContentStore
	HomeFiles         fs.FS
	Intelligence      *IntelligenceService
}

func (s *Server) admin(r *http.Request) (int64, error) {
	u, e := s.identity(r)
	if e != nil {
		return 0, e
	}
	if u.Role != "admin" {
		return 0, errors.New("admin_required")
	}
	return u.ID, nil
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := err.Error()
	switch {
	case errors.Is(err, ErrNotFound):
		status = 404
	case errors.Is(err, ErrConflict), errors.Is(err, ErrSourceLinked), errors.Is(err, ErrUnverifiedSource):
		status = 409
	case errors.Is(err, ErrBusy):
		status = 429
	case errors.Is(err, ErrSourceDeletionReservation):
		status = 503
	case errors.Is(err, ErrCredential), errors.Is(err, ErrUserIDRequired):
	case code == "invalid_name_or_user_id" || code == "https_site_origin_required" || code == "public_site_required" || code == "invalid_alert_threshold_or_unit" || code == "site_or_account_is_immutable_create_another_source" || code == "account_already_monitored":
	default:
		status = 500
		code = "storage_unavailable"
	}
	jsonResponse(w, status, map[string]string{"error": code})
}
func decode(w http.ResponseWriter, r *http.Request, target any) error {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return errors.New("json_required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("single_json_required")
	}
	return nil
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public/runtime", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]string{"main_origin": s.SubOrigin})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if s.Intelligence != nil && !s.Intelligence.Store.healthy() {
			jsonResponse(w, 503, map[string]string{"status": "unhealthy", "component": "intelligence"})
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/balance/sources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"sources": s.Service.List(), "alerts_configured": s.Service.Webhook != ""})
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var input SourceInput
		if err := decode(w, r, &input); err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
			return
		}
		result, err := s.Service.Save(r.PathValue("id"), input, r.Context().Value(adminKey{}).(int64))
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, result)
	}
	mux.HandleFunc("POST /api/balance/sources", save)
	mux.HandleFunc("PUT /api/balance/sources/{id}", save)
	mux.HandleFunc("DELETE /api/balance/sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision int64 `json:"revision"`
		}
		if decode(w, r, &in) != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
			return
		}
		if err := s.deleteWalletSource(r, r.PathValue("id"), in.Revision); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/balance/sources/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		var result Source
		var err error
		if requestID := r.URL.Query().Get("request_id"); requestID != "" {
			if !validBalanceRequestID(requestID) {
				jsonResponse(w, 400, map[string]string{"error": "invalid_request_id"})
				return
			}
			result, err = s.Service.RefreshAsync(r.Context(), r.PathValue("id"), requestID)
		} else {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(balanceOperationTimeout))
			result, err = s.Service.Refresh(r.Context(), r.PathValue("id"))
		}
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	s.balanceAccountRoutes(mux)
	s.studioRoutes(mux)
	s.assetRoutes(mux)
	s.intelligenceRoutes(mux)
	files, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /ops-assets/", http.StripPrefix("/ops-assets/", http.FileServer(http.FS(files))))
	mux.HandleFunc("GET /admin/balances", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.SubOrigin+"/admin/accounts", http.StatusSeeOther)
	})
	if s.Content != nil && s.HomeFiles != nil {
		mux.Handle("GET /home-assets/", http.StripPrefix("/home-assets/", http.FileServer(http.FS(s.HomeFiles))))
		page := func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, s.HomeFiles, "index.html") }
		mux.HandleFunc("GET /{$}", page)
		mux.HandleFunc("GET /brand-home", page)
		mux.HandleFunc("GET /admin/home", page)
		mux.HandleFunc("GET /public/home", func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 200, map[string]any{"config": s.Content.Published(), "main_origin": s.SubOrigin})
		})
		mux.HandleFunc("GET /api/home", func(w http.ResponseWriter, r *http.Request) { s.homeResponse(w, s.Content.Read()) })
		mux.HandleFunc("POST /api/home/{action}", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Revision int64        `json:"revision"`
				Version  int64        `json:"version"`
				Config   *HomeContent `json:"config"`
			}
			if decode(w, r, &in) != nil {
				jsonResponse(w, 400, map[string]string{"error": "invalid_json"})
				return
			}
			state, err := s.Content.Change(r.PathValue("action"), in.Revision, in.Version, r.Context().Value(adminKey{}).(int64), in.Config)
			if err != nil {
				status := 400
				if errors.Is(err, ErrConflict) {
					status = 409
				}
				if errors.Is(err, ErrNotFound) {
					status = 404
				}
				jsonResponse(w, status, map[string]string{"error": "home_update_failed"})
				return
			}
			s.homeResponse(w, state)
		})
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/balances", http.StatusSeeOther)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if (r.Method == "GET" || r.Method == "HEAD") && s.HomeFiles != nil && hashedPortalAsset.MatchString(r.URL.Path) {
			if info, err := fs.Stat(s.HomeFiles, strings.TrimPrefix(r.URL.Path, "/home-assets/")); err == nil && info.Mode().IsRegular() {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		frame := "; frame-src 'none'"
		origins := []string{}
		for _, raw := range []string{s.StudioURL, s.CanvasURL} {
			if u, e := url.Parse(raw); e == nil && u.Host != "" {
				origins = append(origins, u.Scheme+"://"+u.Host)
			}
		}
		if len(origins) > 0 {
			frame = "; frame-src " + strings.Join(origins, " ")
		}
		if r.URL.Path == "/intelligence-tests" {
			frame = "; frame-src blob:"
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' https: blob:; base-uri 'none'; form-action 'none'; frame-ancestors "+s.SubOrigin+frame)
		if r.URL.Path == "/intelligence-tests" {
			// Only this new surface renders opaque-origin generated HTML previews.
			// Child policies block all network; the page itself can only reach Main.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data: blob:; base-uri 'none'; form-action 'none'; object-src 'none'; frame-src blob:; frame-ancestors "+s.SubOrigin)
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.PublicOrigin {
				jsonResponse(w, 403, map[string]string{"error": "origin_not_allowed"})
				return
			}
			user, err := s.identity(r)
			if err == nil && !strings.HasPrefix(r.URL.Path, "/api/studio/") && !strings.HasPrefix(r.URL.Path, "/api/canvas/") && !intelligenceReadRoute(r) && user.Role != "admin" {
				err = errors.New("admin_required")
			}
			id := user.ID
			if err != nil {
				code := 401
				if err.Error() == "admin_required" {
					code = 403
				}
				if err.Error() == "main_identity_unavailable" {
					code = 503
				}
				jsonResponse(w, code, map[string]string{"error": err.Error()})
				return
			}
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), adminKey{}, id), identityKey{}, user))
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) deleteWalletSource(r *http.Request, id string, revision int64) error {
	if s.Service.Bridge == nil {
		return s.Service.Delete(id, revision)
	}
	current, ok := s.Service.Store.snapshot()[id]
	if !ok {
		return ErrNotFound
	}
	if current.Source.Revision != revision {
		return ErrConflict
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if err := s.Service.Bridge.Push(ctx, s.Service.Store); err != nil {
		return err
	}
	var reservation sourceDeletionReservation
	input := map[string]any{"source_id": id, "source_revision": revision}
	if err := s.mainAPI(r, http.MethodPost, walletSourceDeletionPath, input, "", &reservation); err != nil {
		return err
	}
	if err := s.Service.deleteReservedSource(id, revision, reservation); err != nil {
		// Keep the Main reservation on conflict, failed persistence, or an
		// unknown local outcome. A retry or a newer edit is the recovery path.
		return err
	}
	// Durable local deletion has completed. If this best-effort sync fails,
	// Main's reservation still blocks linking until the next periodic snapshot.
	_ = s.Service.Bridge.Push(ctx, s.Service.Store)
	return nil
}

type adminKey struct{}

func (s *Server) homeResponse(w http.ResponseWriter, state HomeState) {
	versions := make([]map[string]any, 0, len(state.Versions))
	for i := len(state.Versions) - 1; i >= 0; i-- {
		v := state.Versions[i]
		versions = append(versions, map[string]any{"id": v.ID, "published_at": v.PublishedAt, "published_by": v.PublishedBy})
	}
	jsonResponse(w, 200, map[string]any{"config": state.Draft, "revision": state.Revision, "versions": versions, "main_origin": s.SubOrigin})
}
