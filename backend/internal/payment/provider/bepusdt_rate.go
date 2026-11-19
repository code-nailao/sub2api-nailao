package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
)

const binanceFiatReferenceURL = "https://www.binance.com/bapi/asset/v1/public/asset-service/product/currency"

// binanceReferenceRate reads Binance's public CNY-per-USD reference. Opting in
// explicitly adopts that number as the merchant's CNY-per-USDT pricing reference;
// it is not a C2C/Convert execution quote or a claim of USDT/USD parity. The
// gateway still allocates the exact payable amount and locks it on the order.
// Fetch only for new orders, never when querying or fulfilling an existing order.
func (b *BEPusdt) binanceReferenceRate(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binanceFiatReferenceURL, nil)
	if err != nil {
		return "", fmt.Errorf("bepusdt: Binance fiat reference request invalid")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("bepusdt: Binance fiat reference temporarily unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bepusdt: Binance fiat reference HTTP status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return "", fmt.Errorf("bepusdt: Binance fiat reference response invalid")
	}
	var result struct {
		Code    string `json:"code"`
		Success bool   `json:"success"`
		Data    []struct {
			Pair string      `json:"pair"`
			Rate json.Number `json:"rate"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Code != "000000" || !result.Success {
		return "", fmt.Errorf("bepusdt: Binance fiat reference response invalid")
	}
	rate := ""
	for _, row := range result.Data {
		if row.Pair != "CNY_USD" {
			continue
		}
		if rate != "" || len(row.Rate) > 24 || !bepDecimal.MatchString(row.Rate.String()) {
			return "", fmt.Errorf("bepusdt: Binance CNY reference invalid or ambiguous")
		}
		n, err := decimal.NewFromString(row.Rate.String())
		// A bounded merchant sanity check, not a replacement rate: unexpected
		// units/schema or a discontinuous reference must stop new orders.
		if err != nil || n.LessThan(decimal.NewFromInt(1)) || n.GreaterThan(decimal.NewFromInt(20)) || n.Exponent() < -8 {
			return "", fmt.Errorf("bepusdt: Binance CNY reference outside supported range")
		}
		rate = n.String()
	}
	if rate == "" {
		return "", fmt.Errorf("bepusdt: Binance CNY reference missing")
	}
	return rate, nil
}
