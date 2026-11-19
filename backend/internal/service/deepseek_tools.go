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
type DeepSeekToolUsage struct {
	SearchCount         int     `json:"search_count"`
	FetchCount          int     `json:"fetch_count"`
	SearchPrice         float64 `json:"search_price_per_call"`
	FetchPrice          float64 `json:"fetch_price_per_call"`
	SupplierAccountID   int64   `json:"supplier_account_id,omitempty"`
	SupplierSearchPrice float64 `json:"supplier_search_price_per_call"`
	SupplierFetchPrice  float64 `json:"supplier_fetch_price_per_call"`
	Standalone          bool    `json:"standalone,omitempty"`
	SupplierHasQuota    bool    `json:"-"`
	SupplierDailyQuota  bool    `json:"-"`
	SupplierWeeklyQuota bool    `json:"-"`
}

func (u *DeepSeekToolUsage) Cost() float64 {
	if u == nil {
		return 0
	}
	return QuantizeUsageBillingAmount(float64(u.SearchCount)*u.SearchPrice + float64(u.FetchCount)*u.FetchPrice)
}
func (u *DeepSeekToolUsage) SupplierCost() float64 {
	if u == nil {
		return 0
	}
	return QuantizeUsageBillingAmount(float64(u.SearchCount)*u.SupplierSearchPrice + float64(u.FetchCount)*u.SupplierFetchPrice)
}

// BuildDeepSeekToolsURL accepts an account API root or an explicit tools root.
// Custom prefixes and version suffixes are preserved by the existing URL builder.
func BuildDeepSeekToolsURL(base, operation string) (string, error) {
	if operation != "search" && operation != "fetch" {
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

func (s *OpenAIGatewayService) deepSeekToolAccount(ctx context.Context, key *APIKey, modelAccount *Account) (*Account, func(), error) {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, nil, errors.New("DeepSeek tools require standard billing mode")
	}
	if key == nil || key.Group == nil || key.GroupID == nil || !key.Group.DeepSeekTools.Enabled {
		return nil, nil, errors.New("DeepSeek tools are disabled for this group")
	}
	if err := ValidateDeepSeekToolsConfig(key.Group.Platform, key.Group.SubscriptionType, key.Group.DeepSeekTools, key.Group.SearchPricePer1k); err != nil {
		return nil, nil, err
	}
	selected := modelAccount
	var release func()
	var err error
	id := key.Group.DeepSeekTools.AccountID
	if id != nil {
		selected, err = s.accountRepo.GetByID(ctx, *id)
	} else if selected == nil {
		choice, e := s.SelectAccountWithLoadAwareness(ctx, key.GroupID, "", "", nil)
		err = e
		if choice != nil {
			selected = choice.Account
			release = choice.ReleaseFunc
		}
	}
	if err != nil || selected == nil {
		if release != nil {
			release()
		}
		return nil, nil, errors.New("DeepSeek tool account unavailable")
	}
	// Reload on every operation: moving an account out of the group or disabling
	// it takes effect before another paid tool is sent.
	if s.accountRepo != nil {
		selected, err = s.accountRepo.GetByID(ctx, selected.ID)
	}
	if err != nil || validateDeepSeekToolAccount(key, selected) != nil {
		if release != nil {
			release()
		}
		return nil, nil, errors.New("DeepSeek tool account is disabled, exhausted, or outside this group")
	}
	if release == nil && (modelAccount == nil || selected.ID != modelAccount.ID) && s.concurrencyService != nil {
		slot, e := s.concurrencyService.AcquireAccountSlot(ctx, selected.ID, selected.Concurrency)
		if e != nil || slot == nil || !slot.Acquired {
			return nil, nil, errors.New("DeepSeek tool account is busy")
		}
		release = slot.ReleaseFunc
	}
	if release == nil {
		release = func() {}
	}
	return selected, release, nil
}

func deepSeekToolSnapshot(key *APIKey, account *Account) (*DeepSeekToolUsage, error) {
	u := &DeepSeekToolUsage{SearchPrice: *key.Group.SearchPricePer1k / 1000, FetchPrice: *key.Group.DeepSeekTools.FetchPricePer1K / 1000, SupplierAccountID: account.ID, SupplierHasQuota: account.HasAnyQuotaLimit(), SupplierDailyQuota: account.GetQuotaDailyLimit() > 0, SupplierWeeklyQuota: account.GetQuotaWeeklyLimit() > 0}
	for _, item := range []struct {
		name string
		out  *float64
	}{{"deepseek_search_cost_per_call", &u.SupplierSearchPrice}, {"deepseek_fetch_cost_per_call", &u.SupplierFetchPrice}} {
		val, ok := account.Extra[item.name].(float64)
		if !ok || !validDeepSeekPrice(val) {
			return nil, errors.New("configure DeepSeek tool supplier costs before enabling tools")
		}
		*item.out = val
	}
	return u, nil
}

// ExecuteDeepSeekTool does one supplier operation, never retries it, and never
// bills. The standalone handler and hosted loop settle through RecordUsage.
func (s *OpenAIGatewayService) ExecuteDeepSeekTool(ctx context.Context, account *Account, operation string, body []byte) ([]byte, bool, error) {
	normalized, err := ValidateDeepSeekToolRequest(operation, body)
	if err != nil {
		return nil, false, err
	}
	base := account.GetBaseURL()
	if override, ok := account.Extra["deepseek_tools_base_url"].(string); ok && strings.TrimSpace(override) != "" {
		base = override
	}
	target, err := BuildDeepSeekToolsURL(base, operation)
	if err != nil {
		return nil, false, err
	}
	if _, err = s.validateUpstreamBaseURL(target); err != nil {
		return nil, false, err
	}
	token := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if token == "" {
		return nil, false, errors.New("DeepSeek tool account has no API key")
	}
	toolCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(toolCtx, HTTPUpstreamProfileOpenAI)), http.MethodPost, target, bytes.NewReader(normalized))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
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
	if account == nil || account.Platform != PlatformDeepseek || account.Type != AccountTypeAPIKey || !account.IsSchedulable() || !slices.Contains(account.GroupIDs, *key.GroupID) || account.Extra["deepseek_tools_enabled"] != true {
		return errors.New("DeepSeek tool account is disabled, exhausted, or outside this group")
	}
	return nil
}

func (s *OpenAIGatewayService) PrepareDeepSeekTool(ctx context.Context, key *APIKey) (*Account, *DeepSeekToolUsage, func(), error) {
	account, release, err := s.deepSeekToolAccount(ctx, key, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	usage, err := deepSeekToolSnapshot(key, account)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	usage.Standalone = true
	return account, usage, release, nil
}
