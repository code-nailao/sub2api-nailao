package domain

// DeepSeekToolsConfig contains group policy only. Credentials belong to accounts.
// A nil price is unconfigured; an explicit zero is free.
type DeepSeekToolsConfig struct {
	Enabled         bool     `json:"enabled"`
	AutoInject      bool     `json:"auto_inject"`
	AccountID       *int64   `json:"account_id,omitempty"`
	FetchPricePer1K *float64 `json:"fetch_price_per_1k"`
}
