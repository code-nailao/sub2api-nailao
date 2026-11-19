package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	_ "golang.org/x/image/webp"
	"sources.local/mediaengine"
)

func newSourcesImageDownloadClient(cfg *config.Config) (*http.Client, error) {
	allowPrivate := cfg != nil && cfg.Security.URLAllowlist.AllowPrivateHosts
	client, err := httpclient.GetClient(httpclient.Options{Timeout: 40 * time.Second, ResponseHeaderTimeout: 10 * time.Second, ValidateResolvedIP: true, AllowPrivateHosts: allowPrivate, MaxConnsPerHost: 4})
	if err != nil {
		return nil, err
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone, nil
}
func (s *SourcesMediaEngine) imageBytes(ctx context.Context, ref string, max int64) ([]byte, string, error) {
	var data []byte
	if strings.HasPrefix(ref, "data:") {
		header, encoded, ok := strings.Cut(ref, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, "", errors.New("image reference must be base64 PNG, JPEG or WebP")
		}
		if int64(base64.StdEncoding.DecodedLen(len(encoded))) > max {
			return nil, "", errors.New("image reference is too large")
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, "", errors.New("invalid image reference encoding")
		}
	} else {
		u, err := url.Parse(ref)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return nil, "", errors.New("image reference must use HTTPS")
		}
		allowPrivate := s.gateway.cfg != nil && s.gateway.cfg.Security.URLAllowlist.AllowPrivateHosts
		if _, err = urlvalidator.ValidateHTTPSURL(ref, urlvalidator.ValidationOptions{AllowPrivate: allowPrivate}); err != nil {
			return nil, "", errors.New("image reference host is not allowed")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := s.downloadClient.Do(req)
		if err != nil {
			return nil, "", errors.New("image download unavailable")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			return nil, "", errors.New("image download not successful")
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return nil, "", errors.New("image download incomplete")
		}
	}
	if len(data) == 0 || int64(len(data)) > max {
		return nil, "", errors.New("image exceeds byte limit")
	}
	info, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || info.Width <= 0 || info.Height <= 0 || int64(info.Width)*int64(info.Height) > 100000000 {
		return nil, "", errors.New("invalid image dimensions or format")
	}
	if _, decoded, e := image.Decode(bytes.NewReader(data)); e != nil || decoded != format {
		return nil, "", errors.New("incomplete image data")
	}
	mime := ""
	switch format {
	case "png":
		mime = "image/png"
	case "jpeg":
		mime = "image/jpeg"
	case "webp":
		mime = "image/webp"
	default:
		return nil, "", errors.New("image must be PNG, JPEG or WebP")
	}
	return data, mime, nil
}

// SourcesImageObjectStorage keeps opaque object identities in the encrypted
// task record, not expiring signed URLs. Ordinary image storage remains unchanged.
type SourcesImageObjectStorage interface {
	ImageStorage
	SourcesImageURL(context.Context, string, time.Duration) (string, error)
	SourcesImageStoreID() string
}
type sourcesImageObject struct {
	StoreID string `json:"store_id"`
	Key     string `json:"key"`
}

func (s *SourcesMediaEngine) objectStorage() (SourcesImageObjectStorage, *ImageResultUploader, error) {
	u, enabled := s.images.current()
	if !enabled || u == nil {
		return nil, nil, errors.New("native image storage unavailable")
	}
	storage, ok := u.storage.(SourcesImageObjectStorage)
	if !ok {
		return nil, nil, errors.New("media engine requires renewable object URLs")
	}
	return storage, u, nil
}
func (s *SourcesMediaEngine) objectURL(ctx context.Context, ref sourcesImageObject, ttl time.Duration) (string, error) {
	storage, _, err := s.objectStorage()
	if err != nil {
		return "", err
	}
	if ref.StoreID != storage.SourcesImageStoreID() {
		return "", errors.New("original task storage configuration required")
	}
	return storage.SourcesImageURL(ctx, ref.Key, ttl)
}
func (s *SourcesMediaEngine) stageInputs(ctx context.Context, r *sourcesMediaEngineRequest, operation string) ([]string, []sourcesImageObject, error) {
	storage, u, err := s.objectStorage()
	if err != nil {
		return nil, nil, err
	}
	refs := []string{}
	objects := []sourcesImageObject{}
	total := int64(0)
	for i, ref := range r.refs {
		data, ct, err := s.imageBytes(ctx, ref, 20<<20)
		if err != nil {
			return nil, nil, err
		}
		total += int64(len(data))
		if total > 20<<20 {
			return nil, nil, errors.New("image reference total exceeds 20 MiB")
		}
		key := u.buildKey(sourcesMediaEngineObjectRoot(s.ns.Name())+"input-"+operation+"-"+r.Hash, i, ct)
		if _, err = storage.Save(ctx, key, ct, data); err != nil {
			return nil, nil, errors.New("image reference storage unavailable")
		}
		object := sourcesImageObject{StoreID: storage.SourcesImageStoreID(), Key: key}
		url, err := s.objectURL(ctx, object, 48*time.Hour)
		if err != nil {
			return nil, nil, err
		}
		objects = append(objects, object)
		refs = append(refs, url)
	}
	return refs, objects, nil
}
func (s *SourcesMediaEngine) PrepareSubmit(ctx context.Context, t *mediaengine.Task) error {
	ticket, err := sourcesImageTicket(t)
	if err != nil {
		return err
	}
	if len(ticket.Inputs) == 0 {
		return nil
	}
	refs := make([]string, 0, len(ticket.Inputs))
	for _, object := range ticket.Inputs {
		url, err := s.objectURL(ctx, object, 48*time.Hour)
		if err != nil {
			return err
		}
		refs = append(refs, url)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(t.Body, &payload) != nil {
		return mediaengine.ErrConflict
	}
	payload["images"], err = json.Marshal(refs)
	if err != nil {
		return err
	}
	t.Body, err = json.Marshal(payload)
	return err
}
func (s *SourcesMediaEngine) StoreResult(ctx context.Context, t *mediaengine.Task, urls []string) (json.RawMessage, error) {
	if len(urls) != 1 {
		return nil, mediaengine.ErrConflict
	}
	storage, u, err := s.objectStorage()
	if err != nil {
		return nil, err
	}
	data, ct, err := s.imageBytes(ctx, urls[0], 32<<20)
	if err != nil {
		return nil, err
	}
	key := u.buildKey(sourcesMediaEngineObjectRoot(s.ns.Name())+"result-"+t.ID, 0, ct)
	if _, err = storage.Save(ctx, key, ct, data); err != nil {
		return nil, errors.New("image result storage unavailable")
	}
	return json.Marshal(sourcesImageObject{StoreID: storage.SourcesImageStoreID(), Key: key})
}

// Result signs only the already-owned object; no new generation or settlement.
func (s *SourcesMediaEngine) Result(ctx context.Context, t *mediaengine.Task) (json.RawMessage, error) {
	var object sourcesImageObject
	if t.Phase != "completed" || json.Unmarshal(t.Result, &object) != nil || object.Key == "" {
		return nil, mediaengine.ErrConflict
	}
	url, err := s.objectURL(ctx, object, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"created": t.CreatedAt, "data": []map[string]string{{"url": url}}})
}
