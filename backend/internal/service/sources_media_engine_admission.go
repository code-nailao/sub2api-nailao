package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"sources.local/mediaengine"
)

// The first migration has one explicitly configured private destination. This
// client never follows redirects, uses environment proxies, or retries a paid
// POST. Normal split deployments do not configure this temporary bridge.
type sourcesLegacyAdmission struct {
	origin *url.URL
	client *http.Client
}

type SourcesLegacyAdmissionResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

func newSourcesLegacyAdmission(origin string) (*sourcesLegacyAdmission, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return nil, errors.New("legacy admission origin must be a fixed private HTTP IP and port")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return nil, errors.New("legacy admission origin must be a fixed private HTTP IP and port")
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		DisableKeepAlives: true, ResponseHeaderTimeout: 95 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	}
	return &sourcesLegacyAdmission{origin: u, client: &http.Client{
		Transport: transport, Timeout: 100 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (s *SourcesMediaEngine) configureAdmission() error {
	path, origin := os.Getenv("SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE"), os.Getenv("SOURCES_MEDIA_ENGINE_LEGACY_ORIGIN")
	if path == "" && origin == "" {
		return nil
	}
	if path == "" || processrole.BackgroundEnabled() || s.admissionReadOnly {
		return errors.New("controlled admission requires one serving process and an explicit journal")
	}
	gate, legacy, err := openSourcesAdmission(path, origin)
	if err != nil {
		return err
	}
	s.admission, s.legacyAdmission = gate, legacy
	processrole.Default().AttachAdmission(gate, s.taskProducer().ReleaseAdmissionHold, s.taskProducer().AdmissionHoldCount)
	return nil
}

func openSourcesAdmission(path, origin string) (*processrole.AdmissionGate, *sourcesLegacyAdmission, error) {
	var legacy *sourcesLegacyAdmission
	if origin != "" {
		var err error
		legacy, err = newSourcesLegacyAdmission(origin)
		if err != nil {
			return nil, nil, err
		}
	}
	gate, err := processrole.OpenAdmissionGate(path)
	if err != nil {
		return nil, nil, err
	}
	if gate.Status().Mode == processrole.AdmissionLegacy && legacy == nil {
		_ = gate.Close()
		return nil, nil, errors.New("legacy admission requires an explicit private origin")
	}
	return gate, legacy, nil
}

func (s *SourcesMediaEngine) BeginAdmission(ctx context.Context) (*processrole.AdmissionLease, error) {
	if s.admission != nil {
		return s.admission.Begin(ctx)
	}
	return &processrole.AdmissionLease{Mode: processrole.AdmissionEnabled}, nil
}

func (s *SourcesMediaEngine) ForwardLegacyAdmission(ctx context.Context, key *APIKey, endpoint string, headers http.Header, body []byte, idem, clientIP string) (*SourcesLegacyAdmissionResponse, bool, error) {
	if s.legacyAdmission == nil || key == nil || key.GroupID == nil || len(body) == 0 || idem == "" {
		return nil, true, mediaengine.ErrDenied // No HTTP request was sent.
	}
	switch endpoint {
	case "/v1/images/generations/async", "/v1/images/edits/async", "/images/generations/async", "/images/edits/async":
	default:
		return nil, true, mediaengine.ErrDenied
	}
	parsed, err := parseSourcesMediaEngineRequest(endpoint, headers.Get("Content-Type"), body)
	if err != nil {
		return nil, true, err
	}
	u := *s.legacyAdmission.origin
	u.Path = endpoint
	// A non-replayable body plus a fresh connection prevents net/http's implicit
	// retry for requests carrying an Idempotency-Key header.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, true, err
	}
	req.ContentLength, req.GetBody = int64(len(body)), nil
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Content-Type", "User-Agent"} {
		if value := headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	req.Header.Set("Idempotency-Key", idem)
	if ip := net.ParseIP(clientIP); ip != nil {
		req.Header.Set("X-Forwarded-For", ip.String())
		req.Header.Set("X-Real-IP", ip.String())
	}
	resp, err := s.legacyAdmission.client.Do(req)
	if err != nil {
		return nil, false, errors.New("legacy image admission response unknown; lookup the idempotency key before retrying")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || !json.Valid(raw) {
		return nil, false, errors.New("legacy image admission response incomplete; lookup the idempotency key before retrying")
	}
	known := false
	if resp.StatusCode == http.StatusAccepted {
		var accepted struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(raw, &accepted) == nil && accepted.ID != "" && accepted.ID == accepted.TaskID {
			owner := mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}
			task, lookupErr := s.store.Existing(owner, idem)
			known = lookupErr == nil && task.ID == accepted.ID && task.PayloadHash == parsed.Hash
		}
	} else {
		// These native rejections precede Create, or report a durable conflict.
		switch resp.StatusCode {
		case 400, 401, 403, 404, 409, 413, 422, 429:
			known = true
		}
	}
	result := &SourcesLegacyAdmissionResponse{Status: resp.StatusCode, Body: raw, Headers: map[string]string{
		"Cache-Control": "no-store", "Idempotency-Key": idem,
	}}
	for _, name := range []string{"Location", "Retry-After"} {
		if value := resp.Header.Get(name); value != "" {
			result.Headers[name] = value
		}
	}
	return result, known, nil
}
