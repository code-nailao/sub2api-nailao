package domain

// DeepSeekToolsConfig contains group policy only. Credentials belong to accounts.
// A nil price is unconfigured; an explicit zero is free.
//
// Search and Fetch must both be priced to enable the tools. The standalone
// document and speech tools are optional: a nil price means that tool is not
// offered by the group. Prices are fixed per unit and never multiplied by the
// group or user rate.
type DeepSeekToolsConfig struct {
	Enabled         bool     `json:"enabled"`
	AutoInject      bool     `json:"auto_inject"`
	AccountID       *int64   `json:"account_id,omitempty"`
	FetchPricePer1K *float64 `json:"fetch_price_per_1k"`
	// OCRPricePer1KPages is charged per recognized page.
	OCRPricePer1KPages *float64 `json:"ocr_price_per_1k_pages,omitempty"`
	// ASRPricePerHour is charged per started second of recognized audio.
	ASRPricePerHour *float64 `json:"asr_price_per_hour,omitempty"`
	// TTSPricePer10KChars is charged per billable character of the input text,
	// where a Han character counts twice.
	TTSPricePer10KChars *float64 `json:"tts_price_per_10k_chars,omitempty"`
}
