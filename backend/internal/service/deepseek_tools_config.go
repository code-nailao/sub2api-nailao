package service

import (
	"fmt"
	"math"
)

func ValidateDeepSeekToolsConfig(platform, subscription string, cfg DeepSeekToolsConfig, searchPrice *float64) error {
	if cfg.AccountID != nil && *cfg.AccountID <= 0 {
		return fmt.Errorf("deepseek_tools.account_id must be positive")
	}
	for _, price := range []*float64{searchPrice, cfg.FetchPricePer1K} {
		if price != nil && (math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0) {
			return fmt.Errorf("DeepSeek tool prices must be finite and non-negative")
		}
	}
	if !cfg.Enabled {
		return nil
	}
	if platform != PlatformDeepseek || subscription != SubscriptionTypeStandard {
		return fmt.Errorf("DeepSeek tools require a DeepSeek standard group")
	}
	if searchPrice == nil || cfg.FetchPricePer1K == nil {
		return fmt.Errorf("configure both DeepSeek Search and Fetch prices before enabling tools")
	}
	return nil
}

func cloneDeepSeekToolsConfig(c DeepSeekToolsConfig) DeepSeekToolsConfig {
	if c.AccountID != nil {
		v := *c.AccountID
		c.AccountID = &v
	}
	if c.FetchPricePer1K != nil {
		v := *c.FetchPricePer1K
		c.FetchPricePer1K = &v
	}
	return c
}

func (g *Group) DeepSeekToolsPublic() DeepSeekToolsConfig {
	c := cloneDeepSeekToolsConfig(g.DeepSeekTools)
	c.AccountID = nil
	return c
}

func normalizeDeepSeekToolMode(mode string) string {
	if mode == "" {
		return "inherit"
	}
	return mode
}
func validateDeepSeekToolMode(mode string) error {
	switch normalizeDeepSeekToolMode(mode) {
	case "inherit", "enabled", "disabled":
		return nil
	}
	return fmt.Errorf("deepseek_tool_mode must be inherit, enabled or disabled")
}

func (k *APIKey) DeepSeekToolsAutoInject() bool {
	if k == nil || k.Group == nil || k.Group.Platform != PlatformDeepseek || !k.Group.DeepSeekTools.Enabled {
		return false
	}
	switch k.DeepSeekToolMode {
	case "disabled":
		return false
	case "enabled":
		return true
	}
	return k.Group.DeepSeekTools.AutoInject
}

func validDeepSeekPrice(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func ValidateDeepSeekToolAccountConfig(platform, accountType string, extra map[string]any) error {
	enabled, hasEnabled := extra["deepseek_tools_enabled"]
	if hasEnabled {
		if _, ok := enabled.(bool); !ok {
			return fmt.Errorf("deepseek_tools_enabled must be a boolean")
		}
	}
	if enabled != true {
		return nil
	}
	if platform != PlatformDeepseek || accountType != AccountTypeAPIKey {
		return fmt.Errorf("DeepSeek tools require a DeepSeek API key account")
	}
	for _, key := range []string{"deepseek_search_cost_per_call", "deepseek_fetch_cost_per_call"} {
		v, ok := extra[key].(float64)
		if !ok || !validDeepSeekPrice(v) {
			return fmt.Errorf("%s must be configured as a non-negative price", key)
		}
	}
	if value, ok := extra["deepseek_native_tools_enabled"]; ok {
		if _, valid := value.(bool); !valid {
			return fmt.Errorf("deepseek_native_tools_enabled must be a boolean")
		}
	}
	if value, ok := extra["deepseek_tools_base_url"]; ok {
		url, valid := value.(string)
		if !valid {
			return fmt.Errorf("deepseek_tools_base_url must be a URL")
		}
		if url != "" {
			if _, err := BuildDeepSeekToolsURL(url, "search"); err != nil {
				return err
			}
		}
	}
	return nil
}
