package portal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var errAssetInvalid = errors.New("invalid_image")
var assetName = regexp.MustCompile(`^[a-f0-9]{64}\.(png|jpg)$`)

func (s *ContentStore) saveAsset(body []byte) (string, error) {
	s.mu.Lock()
	failed := s.failed
	s.mu.Unlock()
	if failed != nil {
		return "", ErrPersistenceUncertain
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || len(body) > 2<<20 || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 2048 || cfg.Height > 2048 || (format != "png" && format != "jpeg") {
		return "", errAssetInvalid
	}
	if _, _, err = image.Decode(bytes.NewReader(body)); err != nil {
		return "", errAssetInvalid
	}
	suffix := "png"
	if format == "jpeg" {
		suffix = "jpg"
	}
	sum := sha256.Sum256(body)
	name := hex.EncodeToString(sum[:]) + "." + suffix
	dir := filepath.Join(filepath.Dir(s.path), "assets")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(body)
	}
	if err == nil {
		err = temp.Sync()
	}
	if err == nil {
		err = temp.Close()
	}
	if err == nil {
		err = os.Rename(temp.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		return "", err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if err = parent.Sync(); err != nil {
		return "", err
	}
	return "/content-assets/" + name, nil
}
func (s *ContentStore) assetPublished(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := "/content-assets/" + name
	for _, version := range s.state.Versions {
		c := version.Config
		if c.LogoLight == path || c.LogoDark == path {
			return true
		}
		for _, contact := range c.Brand.Contacts {
			if contact.Logo == path || contact.QRImage == path {
				return true
			}
		}
	}
	return false
}
func (s *Server) assetRoutes(mux *http.ServeMux) {
	if s.Content == nil {
		return
	}
	mux.HandleFunc("POST /api/home/assets", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "image/png" && r.Header.Get("Content-Type") != "image/jpeg" {
			jsonResponse(w, 415, map[string]string{"error": "png_or_jpeg_required"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			jsonResponse(w, 413, map[string]string{"error": "image_too_large"})
			return
		}
		path, err := s.Content.saveAsset(body)
		if err != nil {
			if errors.Is(err, errAssetInvalid) {
				jsonResponse(w, 400, map[string]string{"error": "invalid_image_max_2048px"})
			} else {
				jsonResponse(w, 503, map[string]string{"error": "asset_storage_unavailable"})
			}
			return
		}
		jsonResponse(w, 200, map[string]string{"path": path})
	})
	read := func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !assetName.MatchString(name) || strings.HasPrefix(r.URL.Path, "/content-assets/") && !s.Content.assetPublished(name) {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/content-assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("Content-Type", "image/png")
		if strings.HasSuffix(name, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
		}
		http.ServeFile(w, r, filepath.Join(filepath.Dir(s.Content.path), "assets", name))
	}
	mux.HandleFunc("GET /api/home/assets/{name}", read)
	mux.HandleFunc("GET /content-assets/{name}", read)
}
