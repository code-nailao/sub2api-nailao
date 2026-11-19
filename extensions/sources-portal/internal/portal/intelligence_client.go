package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type intelligenceSubmission struct {
	Benchmark string `json:"benchmark"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	Effort    string `json:"reasoning_effort"`
}

type intelligenceRemoteResult struct {
	ID        string    `json:"id"`
	Benchmark string    `json:"benchmark"`
	Status    string    `json:"status"`
	Phase     string    `json:"phase"`
	ExpiresAt time.Time `json:"expires_at"`
	Candy     struct {
		Status       string          `json:"status"`
		Answer       string          `json:"answer"`
		DurationMS   *int64          `json:"duration_ms"`
		InputTokens  *int64          `json:"input_tokens"`
		OutputTokens *int64          `json:"output_tokens"`
		Error        json.RawMessage `json:"error"`
	} `json:"candy"`
	Result struct {
		HTML         string `json:"html"`
		DurationMS   *int64 `json:"duration_ms"`
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	} `json:"result"`
	Assessment struct {
		Quality string `json:"quality"`
	} `json:"assessment"`
	Error json.RawMessage `json:"error"`
}

type intelligenceRemoteError struct {
	Code       string
	Definitive bool
	RetryAfter time.Duration
}

func (e *intelligenceRemoteError) Error() string { return e.Code }

type intelligenceProvider interface {
	Submit(context.Context, intelligenceSubmission, string) (intelligenceRemoteResult, error)
	Poll(context.Context, string) (intelligenceRemoteResult, error)
}

type intelligenceHTTPProvider struct {
	client *http.Client
	origin string
}

var intelligenceRemoteID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,200}$`)

// errIntelligenceProviderOrigin is returned when the detection service origin
// is missing or not a fixed public HTTPS origin. There is no built-in default:
// the operator chooses the service, so the code carries no third-party address.
var errIntelligenceProviderOrigin = errors.New("intelligence_provider_origin_required")

func newIntelligenceProvider(rawOrigin string) (intelligenceProvider, error) {
	origin, err := CanonicalOrigin(rawOrigin)
	if err != nil {
		return nil, errIntelligenceProviderOrigin
	}
	client := PublicClient()
	client.Timeout = 20 * time.Second
	return &intelligenceHTTPProvider{client: client, origin: origin}, nil
}

func intelligenceFailure(raw []byte) string {
	value := strings.ToLower(string(raw))
	for _, fragment := range []string{"insufficient_quota", "insufficient_balance", "api_key_quota", "quota_limit", "daily limit", "限额", "quota_exceeded", "quota_exhausted", "quota limit", "insufficient balance", "余额不足", "额度不足", "额度已", "billing_hard_limit"} {
		if strings.Contains(value, fragment) {
			return "quota_exhausted"
		}
	}
	for _, fragment := range []string{"invalid_api_key", "api_key_expired", "key_expired", "invalid api key", "api key has expired", "密钥已过期", "密钥无效"} {
		if strings.Contains(value, fragment) {
			return "key_unavailable"
		}
	}
	return "test_failed"
}

func (p *intelligenceHTTPProvider) Submit(ctx context.Context, in intelligenceSubmission, operation string) (intelligenceRemoteResult, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return intelligenceRemoteResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.origin+"/api/v1/tests", bytes.NewReader(raw))
	if err != nil {
		return intelligenceRemoteResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", operation)
	// The remote idempotency store is in-memory and scoped to client IP. A lost
	// response cannot safely be replayed after their restart, even with this key.
	req.GetBody = nil
	return p.do(req, true)
}

func (p *intelligenceHTTPProvider) Poll(ctx context.Context, id string) (intelligenceRemoteResult, error) {
	if !intelligenceRemoteID.MatchString(id) {
		return intelligenceRemoteResult{}, &intelligenceRemoteError{Code: "task_unavailable", Definitive: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.origin+"/api/v1/tests/"+url.PathEscape(id), nil)
	if err != nil {
		return intelligenceRemoteResult{}, err
	}
	return p.do(req, false)
}

func (p *intelligenceHTTPProvider) do(req *http.Request, submitting bool) (intelligenceRemoteResult, error) {
	var out intelligenceRemoteResult
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Sources-Intelligence-Monitor/1")
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return out, &intelligenceRemoteError{Code: "request_uncertain"}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return out, &intelligenceRemoteError{Code: "response_uncertain"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := intelligenceFailure(raw)
		if response.StatusCode == 404 && !submitting {
			code = "task_unavailable"
		} else if response.StatusCode == 429 || response.StatusCode == 503 {
			code = "test_service_busy"
		}
		delay := 30 * time.Second
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 300 {
			delay = time.Duration(seconds) * time.Second
		}
		// 5xx and redirects may occur after acceptance. Never call a rejected or
		// indeterminate submission again automatically.
		definitive := response.StatusCode >= 400 && response.StatusCode < 500
		return out, &intelligenceRemoteError{Code: code, Definitive: definitive, RetryAfter: delay}
	}
	if json.Unmarshal(raw, &out) != nil || !intelligenceRemoteID.MatchString(out.ID) || len(out.Result.HTML) > 2<<20 || len(out.Candy.Answer) > 512<<10 {
		return out, &intelligenceRemoteError{Code: "response_uncertain"}
	}
	switch out.Status {
	case "queued", "running", "succeeded", "failed", "cancelled":
	default:
		return out, &intelligenceRemoteError{Code: "response_uncertain"}
	}
	return out, nil
}

func intelligenceErrorCode(err error) (string, bool, time.Duration) {
	var remote *intelligenceRemoteError
	if errors.As(err, &remote) {
		return remote.Code, remote.Definitive, remote.RetryAfter
	}
	return "request_uncertain", false, 30 * time.Second
}
