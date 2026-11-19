package payment

import (
	"errors"
	"strings"
	"time"
)

const (
	TypeBEPusdt              = "bepusdt"
	TypeUSDTBSC              = "usdt_bsc"
	TypeUSDTArbitrum         = "usdt_arbitrum"
	BEPusdtOrderPrefix       = "MAIN_USDT_"
	ProviderStatusConfirming = "confirming"
	BEPusdtRateGateway       = "gateway"
	BEPusdtRateBinance       = "binance_reference"
)

// ParseBEPusdtRateSource also validates disabled draft configurations: invalid
// pricing options must never prevent rebuilding a provider for existing orders.
func ParseBEPusdtRateSource(raw string) (string, error) {
	switch value := strings.TrimSpace(raw); value {
	case "", BEPusdtRateGateway:
		return BEPusdtRateGateway, nil
	case BEPusdtRateBinance:
		return value, nil
	default:
		return "", errors.New("unsupported bepusdt rateSource")
	}
}

var ErrPaymentOutcomeUnknown = errors.New("payment outcome unknown")
var ErrRefundUnsupported = errors.New("automatic refund is not supported by this provider")

// NativeBEPConfig retains payment credentials/protocol settings but discards
// retired presentation settings even when submitted by an old admin client.
func NativeBEPConfig(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if strings.ToLower(k) != "checkoutpresentation" && strings.ToLower(k) != "checkoutcopy" {
			out[k] = v
		}
	}
	return out
}

// CryptoPaymentDetails contains public data only; chain quantities stay decimal
// strings, never float64 or the existing two-decimal fiat database columns.
type CryptoPaymentDetails struct {
	ReferenceRate   string    `json:"reference_rate,omitempty"`
	RateSource      string    `json:"rate_source,omitempty"`
	Asset           string    `json:"asset"`
	Network         string    `json:"network"`
	TradeType       string    `json:"trade_type"`
	TradeID         string    `json:"trade_id,omitempty"`
	Fiat            string    `json:"fiat"`
	FiatAmount      string    `json:"fiat_amount"`
	ChainAmount     string    `json:"chain_amount,omitempty"`
	Address         string    `json:"address,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	Status          string    `json:"status"`
	TransactionHash string    `json:"transaction_hash,omitempty"`
}

func BEPusdtTradeType(method string) (string, string, bool) {
	n, ok := BEPusdtNetworkByMethod(method)
	return n.TradeType, n.Network, ok
}

// ErrInvalidPaymentEvidence distinguishes an invalid gateway response from a transport outage.
var ErrInvalidPaymentEvidence = errors.New("invalid gateway payment evidence")
