package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/tidwall/gjson"
)

const deepSeekToolResponseLimit = 4 << 20

// DeepSeekToolUsage is an immutable, credential-free price and success snapshot.
// It deliberately does not reuse Grok's multiplier-sensitive SearchCount.
//
// Search and Fetch store a per-call price. The standalone document and speech
// tools store the configured price unchanged (per 1,000 pages, per hour, per
// 10,000 characters) next to their confirmed units; those fields are omitted
// while zero, so Search/Fetch snapshots keep their original shape.
type DeepSeekToolUsage struct {
	SearchCount                 int     `json:"search_count"`
	FetchCount                  int     `json:"fetch_count"`
	SearchPrice                 float64 `json:"search_price_per_call"`
	FetchPrice                  float64 `json:"fetch_price_per_call"`
	OCRPages                    int     `json:"ocr_pages,omitempty"`
	ASRSeconds                  int     `json:"asr_seconds,omitempty"`
	TTSCharacters               int     `json:"tts_characters,omitempty"`
	OCRPricePer1KPages          float64 `json:"ocr_price_per_1k_pages,omitempty"`
	ASRPricePerHour             float64 `json:"asr_price_per_hour,omitempty"`
	TTSPricePer10KChars         float64 `json:"tts_price_per_10k_chars,omitempty"`
	SupplierAccountID           int64   `json:"supplier_account_id,omitempty"`
	SupplierSearchPrice         float64 `json:"supplier_search_price_per_call"`
	SupplierFetchPrice          float64 `json:"supplier_fetch_price_per_call"`
	SupplierOCRPricePer1KPages  float64 `json:"supplier_ocr_price_per_1k_pages,omitempty"`
	SupplierASRPricePerHour     float64 `json:"supplier_asr_price_per_hour,omitempty"`
	SupplierTTSPricePer10KChars float64 `json:"supplier_tts_price_per_10k_chars,omitempty"`
	Standalone                  bool    `json:"standalone,omitempty"`
	SupplierHasQuota            bool    `json:"-"`
	SupplierDailyQuota          bool    `json:"-"`
	SupplierWeeklyQuota         bool    `json:"-"`
}

func (u *DeepSeekToolUsage) Cost() float64 {
	if u == nil {
		return 0
	}
	return QuantizeUsageBillingAmount(float64(u.SearchCount)*u.SearchPrice + float64(u.FetchCount)*u.FetchPrice + u.mediaAmount(u.OCRPricePer1KPages, u.ASRPricePerHour, u.TTSPricePer10KChars))
}
func (u *DeepSeekToolUsage) SupplierCost() float64 {
	if u == nil {
		return 0
	}
	return QuantizeUsageBillingAmount(float64(u.SearchCount)*u.SupplierSearchPrice + float64(u.FetchCount)*u.SupplierFetchPrice + u.mediaAmount(u.SupplierOCRPricePer1KPages, u.SupplierASRPricePerHour, u.SupplierTTSPricePer10KChars))
}

func (u *DeepSeekToolUsage) mediaAmount(ocrPer1K, asrPerHour, ttsPer10K float64) float64 {
	return float64(u.OCRPages)*ocrPer1K/1000 + float64(u.ASRSeconds)*asrPerHour/3600 + float64(u.TTSCharacters)*ttsPer10K/10000
}

// usesMediaTools reports whether any standalone document or speech term is set.
// Search/Fetch-only snapshots keep their original billing fingerprint.
func (u *DeepSeekToolUsage) usesMediaTools() bool {
	return u != nil && (u.OCRPages != 0 || u.ASRSeconds != 0 || u.TTSCharacters != 0 ||
		u.OCRPricePer1KPages != 0 || u.ASRPricePerHour != 0 || u.TTSPricePer10KChars != 0 ||
		u.SupplierOCRPricePer1KPages != 0 || u.SupplierASRPricePerHour != 0 || u.SupplierTTSPricePer10KChars != 0)
}

// BuildDeepSeekToolsURL accepts an account API root or an explicit tools root.
// Custom prefixes and version suffixes are preserved by the existing URL builder.
func BuildDeepSeekToolsURL(base, operation string) (string, error) {
	if !IsDeepSeekToolOperation(operation) {
		return "", errors.New("unsupported DeepSeek tool")
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("invalid DeepSeek tools base URL")
	}
	base = strings.TrimRight(u.String(), "/")
	if strings.HasSuffix(base, "/tools") {
		return base + "/" + operation, nil
	}
	return buildOpenAIEndpointURL(base, "/v1/tools/"+operation), nil
}

// ValidateDeepSeekToolRequest only accepts the provider's documented public
// request fields. A caller cannot inject a model, credential, or upstream URL.
func ValidateDeepSeekToolRequest(operation string, body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("tool request must be a JSON object")
	}
	switch operation {
	case "search":
		for k := range fields {
			if k != "query" && k != "count" {
				return nil, fmt.Errorf("unsupported search field: %s", k)
			}
		}
		var query string
		if json.Unmarshal(fields["query"], &query) != nil || strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 128 {
			return nil, errors.New("query must contain 1 to 128 characters")
		}
		count := 10
		if raw, ok := fields["count"]; ok {
			if string(raw) == "null" || json.Unmarshal(raw, &count) != nil || count < 1 || count > 50 {
				return nil, errors.New("count must be an integer from 1 to 50")
			}
		}
		return json.Marshal(map[string]any{"query": strings.TrimSpace(query), "count": count})
	case "fetch":
		for k := range fields {
			if k != "url" {
				return nil, fmt.Errorf("unsupported fetch field: %s", k)
			}
		}
		var target string
		if json.Unmarshal(fields["url"], &target) != nil || len(target) > 8192 {
			return nil, errors.New("invalid fetch URL")
		}
		parsed, err := url.Parse(target)
		if err != nil || parsed.User != nil {
			return nil, errors.New("fetch URL must not contain credentials")
		}
		if _, err := urlvalidator.ValidateHTTPURL(target, true, urlvalidator.ValidationOptions{}); err != nil {
			return nil, errors.New("fetch requires a public HTTP or HTTPS URL")
		}
		return json.Marshal(map[string]string{"url": target})
	}
	return nil, errors.New("unsupported DeepSeek tool")
}

func deepSeekToolSucceeded(operation string, status int, body []byte) bool {
	if status != http.StatusOK || !gjson.ValidBytes(body) || gjson.GetBytes(body, "error").Exists() {
		return false
	}
	if operation == "search" {
		r := gjson.GetBytes(body, "results")
		for _, item := range r.Array() {
			if item.IsObject() {
				if _, err := urlvalidator.ValidateHTTPURL(item.Get("url").String(), true, urlvalidator.ValidationOptions{}); err == nil {
					return true
				}
			}
		}
		return false
	}
	return operation == "fetch" && gjson.GetBytes(body, "content").Type == gjson.String && strings.TrimSpace(gjson.GetBytes(body, "content").String()) != ""
}

// checkDeepSeekToolsEnabled is the group gate shared by hosted and standalone tools.
func (s *OpenAIGatewayService) checkDeepSeekToolsEnabled(key *APIKey) error {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return errors.New("DeepSeek tools require standard billing mode")
	}
	if key == nil || key.Group == nil || key.GroupID == nil || !key.Group.DeepSeekTools.Enabled {
		return errors.New("DeepSeek tools are disabled for this group")
	}
	return ValidateDeepSeekToolsConfig(key.Group.Platform, key.Group.SubscriptionType, key.Group.DeepSeekTools, key.Group.SearchPricePer1k)
}

var errDeepSeekToolAccountUnavailable = errors.New("DeepSeek tool account unavailable")

// deepSeekToolAccount picks the supplier for a hosted tool call: the pinned
// account when the group pins one, otherwise the model account itself, which
// must supply tools (judged on its stored row, not the copy it was scheduled
// with). Hosted calls never borrow another account; when the model account has
// no tools, the tools are unavailable and the caller's fallback applies.
func (s *OpenAIGatewayService) deepSeekToolAccount(ctx context.Context, key *APIKey, modelAccount *Account) (*Account, func(), error) {
	if err := s.checkDeepSeekToolsEnabled(key); err != nil {
		return nil, nil, err
	}
	pin := key.Group.DeepSeekTools.AccountID
	if modelAccount != nil && (pin == nil || *pin == modelAccount.ID) {
		// The model request already holds this account's slot.
		fresh, err := s.reloadDeepSeekToolAccount(ctx, key, modelAccount.ID, modelAccount)
		if err != nil {
			return nil, nil, err
		}
		return fresh, func() {}, nil
	}
	return s.selectDeepSeekToolAccount(ctx, key, nil)
}

// deepSeekToolCapable is the static part of the tool account rule; group
// membership and schedulability are checked by validateDeepSeekToolAccount.
func deepSeekToolCapable(account *Account) bool {
	return account != nil && account.Platform == PlatformDeepseek && account.Type == AccountTypeAPIKey && account.Extra["deepseek_tools_enabled"] == true
}

// Reload on every operation: moving an account out of the group or disabling
// it takes effect before another paid tool is sent.
func (s *OpenAIGatewayService) reloadDeepSeekToolAccount(ctx context.Context, key *APIKey, id int64, fallback *Account) (*Account, error) {
	account := fallback
	if s.accountRepo != nil {
		var err error
		if account, err = s.accountRepo.GetByID(ctx, id); err != nil {
			return nil, errDeepSeekToolAccountUnavailable
		}
	}
	if validateDeepSeekToolAccount(key, account) != nil {
		return nil, errors.New("DeepSeek tool account is disabled, exhausted, or outside this group")
	}
	return account, nil
}

// selectDeepSeekToolAccount chooses among the group's schedulable DeepSeek
// API-key accounts that supply tools, through the ordinary scheduler, never one
// in excluded. A pinned account_id overrides the choice. The returned release
// frees the account slot.
func (s *OpenAIGatewayService) selectDeepSeekToolAccount(ctx context.Context, key *APIKey, excluded map[int64]struct{}) (*Account, func(), error) {
	if pin := key.Group.DeepSeekTools.AccountID; pin != nil {
		if _, skip := excluded[*pin]; skip {
			return nil, nil, errDeepSeekToolAccountUnavailable
		}
		account, err := s.reloadDeepSeekToolAccount(ctx, key, *pin, nil)
		if err != nil {
			return nil, nil, err
		}
		release, err := s.acquireDeepSeekToolSlot(ctx, account)
		if err != nil {
			return nil, nil, err
		}
		return account, release, nil
	}
	// Fixed per-unit tool prices are outside the token profit gate.
	ctx = WithOpenAIProfitControlSuppressed(ctx)
	accounts, err := s.listSchedulableAccounts(ctx, key.GroupID, PlatformDeepseek)
	if err != nil {
		return nil, nil, errDeepSeekToolAccountUnavailable
	}
	skip := make(map[int64]struct{}, len(accounts)+len(excluded))
	for id := range excluded {
		skip[id] = struct{}{}
	}
	// The scheduler snapshot lists a metadata projection without the tool
	// setting, so each candidate is judged on its full account record.
	candidates := 0
	for i := range accounts {
		if _, done := skip[accounts[i].ID]; done {
			continue
		}
		if full, err := s.getSchedulableAccount(ctx, accounts[i].ID); err == nil && deepSeekToolCapable(full) {
			candidates++
		} else {
			skip[accounts[i].ID] = struct{}{}
		}
	}
	busy := false
	for ; candidates > 0; candidates-- {
		selection, _, err := s.SelectAccountWithSchedulerForCapability(ctx, key.GroupID, "", "", "", skip, OpenAIUpstreamTransportHTTPSSE, "", false, false, false, PlatformDeepseek)
		if err != nil || selection == nil || selection.Account == nil {
			break
		}
		id := selection.Account.ID
		skip[id] = struct{}{}
		release := selection.ReleaseFunc
		if !selection.Acquired {
			// Tool calls do not queue behind model traffic; try another account.
			if release, err = s.acquireDeepSeekToolSlot(ctx, selection.Account); err != nil {
				busy = true
				continue
			}
		}
		if release == nil {
			release = func() {}
		}
		account, err := s.reloadDeepSeekToolAccount(ctx, key, id, selection.Account)
		if err != nil {
			release()
			continue
		}
		return account, release, nil
	}
	if busy {
		return nil, nil, errors.New("DeepSeek tool account is busy")
	}
	return nil, nil, errDeepSeekToolAccountUnavailable
}

func (s *OpenAIGatewayService) acquireDeepSeekToolSlot(ctx context.Context, account *Account) (func(), error) {
	if s.concurrencyService == nil {
		return func() {}, nil
	}
	slot, err := s.concurrencyService.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err != nil || slot == nil || !slot.Acquired {
		return nil, errors.New("DeepSeek tool account is busy")
	}
	if slot.ReleaseFunc == nil {
		return func() {}, nil
	}
	return slot.ReleaseFunc, nil
}

// deepSeekToolSnapshot freezes the group's Search/Fetch prices and the
// supplier's costs. A missing supplier cost counts as 0 in profit statistics.
func deepSeekToolSnapshot(key *APIKey, account *Account) (*DeepSeekToolUsage, error) {
	if key == nil || key.Group == nil || key.Group.SearchPricePer1k == nil || key.Group.DeepSeekTools.FetchPricePer1K == nil || account == nil {
		return nil, errors.New("configure both DeepSeek Search and Fetch prices before enabling tools")
	}
	return &DeepSeekToolUsage{
		SearchPrice: *key.Group.SearchPricePer1k / 1000, FetchPrice: *key.Group.DeepSeekTools.FetchPricePer1K / 1000,
		SupplierAccountID:   account.ID,
		SupplierSearchPrice: deepSeekSupplierCost(account, "deepseek_search_cost_per_call"),
		SupplierFetchPrice:  deepSeekSupplierCost(account, "deepseek_fetch_cost_per_call"),
		SupplierHasQuota:    account.HasAnyQuotaLimit(), SupplierDailyQuota: account.GetQuotaDailyLimit() > 0, SupplierWeeklyQuota: account.GetQuotaWeeklyLimit() > 0,
	}, nil
}

// deepSeekToolEndpoint builds one supplier request. Only the account's own base
// URL (or its tools override), API key and proxy are used.
func (s *OpenAIGatewayService) deepSeekToolEndpoint(ctx context.Context, account *Account, operation, contentType, accept string, body []byte) (*http.Request, string, error) {
	base := account.GetBaseURL()
	if override, ok := account.Extra["deepseek_tools_base_url"].(string); ok && strings.TrimSpace(override) != "" {
		base = override
	}
	target, err := BuildDeepSeekToolsURL(base, operation)
	if err != nil {
		return nil, "", err
	}
	if _, err = s.validateUpstreamBaseURL(target); err != nil {
		return nil, "", err
	}
	token := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if token == "" {
		return nil, "", errors.New("DeepSeek tool account has no API key")
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI)), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", accept)
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	return req, proxy, nil
}

// ExecuteDeepSeekTool does one supplier operation, never retries it, and never
// bills. The standalone handler and hosted loop settle through RecordUsage.
func (s *OpenAIGatewayService) ExecuteDeepSeekTool(ctx context.Context, account *Account, operation string, body []byte) ([]byte, bool, error) {
	normalized, err := ValidateDeepSeekToolRequest(operation, body)
	if err != nil {
		return nil, false, err
	}
	toolCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, proxy, err := s.deepSeekToolEndpoint(toolCtx, account, operation, "application/json", "application/json", normalized)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, false, errors.New("DeepSeek tool transport failed; operation was not retried")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, deepSeekToolResponseLimit+1))
	if err != nil || len(data) > deepSeekToolResponseLimit {
		return nil, false, errors.New("incomplete or oversized DeepSeek tool response")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("DeepSeek tool supplier returned HTTP %d", resp.StatusCode)
	}
	if !gjson.ValidBytes(data) {
		return nil, false, errors.New("invalid DeepSeek tool response")
	}
	return data, deepSeekToolSucceeded(operation, resp.StatusCode, data), nil
}

func validateDeepSeekToolAccount(key *APIKey, account *Account) error {
	if !deepSeekToolCapable(account) || !account.IsSchedulable() || !slices.Contains(account.GroupIDs, *key.GroupID) {
		return errors.New("DeepSeek tool account is disabled, exhausted, or outside this group")
	}
	return nil
}
