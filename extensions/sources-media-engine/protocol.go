package mediaengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type Credential struct {
	BaseURL, APIKey, ProxyURL string
	AccountID                 int64
	Concurrency               int
}
type Poll struct {
	Status string
	URLs   []string
}

// Transport supplies the host's reviewed account proxy, URL and concurrency policy.
// This package never constructs an unrestricted client or follows redirects.
type Transport interface {
	Do(*http.Request, Credential) (*http.Response, error)
}
type Client struct{ Transport Transport }

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}$`)

func endpoint(base, ref string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (strings.TrimRight(u.Path, "/") != "" && strings.TrimRight(u.Path, "/") != "/v1") {
		return "", errors.New("invalid fixed media engine endpoint")
	}
	u.Path = "/v1/videos"
	if ref != "" {
		if !refPattern.MatchString(ref) {
			return "", errors.New("invalid upstream task reference")
		}
		u.Path += "/" + ref
	}
	return u.String(), nil
}
func (c Client) request(ctx context.Context, cred Credential, method, ref string, body []byte, idem string) (map[string]json.RawMessage, error) {
	if c.Transport == nil || strings.TrimSpace(cred.APIKey) == "" {
		return nil, errors.New("media engine transport unavailable")
	}
	target, err := endpoint(cred.BaseURL, ref)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	// A vendor need not implement Idempotency-Key. Prevent net/http from
	// replaying a POST after a reused connection loses its response.
	if method == http.MethodPost {
		req.GetBody = nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.Transport.Do(req, cred)
	if err != nil {
		if errors.Is(err, ErrDeferred) {
			return nil, ErrDeferred
		}
		return nil, errors.New("media engine request outcome unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 400, 401, 403, 404, 413, 422:
			return nil, ErrRejected
		}
		return nil, errors.New("media engine response not successful")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("media engine response invalid")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil || data == nil {
		return nil, errors.New("media engine response invalid")
	}
	return data, nil
}
func text(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		var s string
		if json.Unmarshal(m[k], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}
func nested(m map[string]json.RawMessage) map[string]json.RawMessage {
	var d map[string]json.RawMessage
	_ = json.Unmarshal(m["data"], &d)
	return d
}
func (c Client) Submit(ctx context.Context, cred Credential, body json.RawMessage, idem string) (string, error) {
	m, err := c.request(ctx, cred, http.MethodPost, "", body, idem)
	if err != nil {
		return "", err
	}
	ref := text(m, "id", "task_id")
	if ref == "" {
		ref = text(nested(m), "id", "task_id")
	}
	if !refPattern.MatchString(ref) {
		return "", errors.New("media engine accepted task reference missing")
	}
	return ref, nil
}
func (c Client) Poll(ctx context.Context, cred Credential, ref string) (Poll, error) {
	m, err := c.request(ctx, cred, http.MethodGet, ref, nil, "")
	if err != nil {
		return Poll{}, err
	}
	d := nested(m)
	status := strings.ToLower(text(m, "status"))
	if status == "" {
		status = strings.ToLower(text(d, "status"))
	}
	switch status {
	case "queued", "pending", "created", "processing", "running", "in_progress", "in-progress", "submitted":
		return Poll{Status: "processing"}, nil
	case "failed", "error", "cancelled", "canceled", "expired":
		return Poll{Status: "failed"}, nil
	case "completed", "complete", "succeeded", "success", "done":
	default:
		return Poll{}, errors.New("unknown media engine task state")
	}

	// data[] defines logical images; root and nested compatibility URLs can
	// carry different signatures for the same image and must not be counted twice.
	items := []map[string]json.RawMessage{}
	_ = json.Unmarshal(m["data"], &items)
	candidates := []map[string]json.RawMessage{d, m}
	if len(items) > 0 {
		if len(items) != 1 {
			return Poll{}, errors.New("media engine result must contain one image")
		}
		candidates = items
	}
	baseText, _ := endpoint(cred.BaseURL, ref)
	base, _ := url.Parse(baseText)
	for _, item := range candidates {
		for _, field := range []string{"url", "image_url", "content_url"} {
			raw := strings.TrimSpace(text(item, field))
			if raw == "" {
				continue
			}
			u, e := url.Parse(raw)
			if e != nil {
				continue
			}
			u = base.ResolveReference(u)
			if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
				continue
			}
			return Poll{Status: "completed", URLs: []string{u.String()}}, nil
		}
	}
	return Poll{}, errors.New("media engine result URL missing or invalid")
}

func ValidateCredential(c Credential) error {
	if c.AccountID <= 0 || strings.TrimSpace(c.APIKey) == "" {
		return errors.New("incomplete media engine credential")
	}
	_, err := endpoint(c.BaseURL, "")
	return err
}
