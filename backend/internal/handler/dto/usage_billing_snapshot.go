package dto

import "github.com/Wei-Shaw/sub2api/internal/service"

// A customer's receipt describes their charge, not supplier policy, costs or
// raw HTTP evidence. Constructing this separate type also prevents future audit
// fields from leaking when the internal snapshot gains fields.
type userContentRejectionBillingSnapshot struct {
	ChargeKind  string  `json:"charge_kind"`
	FixedCharge float64 `json:"fixed_charge"`
}

func usageBillingSnapshotForUser(snapshot *service.UsageBillingSnapshot) any {
	if snapshot == nil {
		return nil
	}
	if snapshot.ChargeKind == "upstream_rejection" || snapshot.ChargeKind == "content_rejection" {
		return &userContentRejectionBillingSnapshot{
			ChargeKind: "content_rejection", FixedCharge: snapshot.FixedCharge,
		}
	}
	if u := snapshot.DeepSeekTools; u != nil {
		// Supplier identity and procurement prices are admin-only.
		tools := map[string]any{"search_count": u.SearchCount, "fetch_count": u.FetchCount,
			"search_price_per_call": u.SearchPrice, "fetch_price_per_call": u.FetchPrice,
			"standalone": u.Standalone}
		// The document and speech tools appear only on their own receipts.
		for _, item := range []struct {
			unitsKey, priceKey string
			count              int
			price              float64
		}{
			{"ocr_pages", "ocr_price_per_1k_pages", u.OCRPages, u.OCRPricePer1KPages},
			{"asr_seconds", "asr_price_per_hour", u.ASRSeconds, u.ASRPricePerHour},
			{"tts_characters", "tts_price_per_10k_chars", u.TTSCharacters, u.TTSPricePer10KChars},
		} {
			if item.count != 0 || item.price != 0 {
				tools[item.unitsKey], tools[item.priceKey] = item.count, item.price
			}
		}
		return map[string]any{"version": snapshot.Version, "charge_kind": snapshot.ChargeKind, "deepseek_tools": tools}
	}
	// Existing ordinary token/tier receipts retain their established shape.
	return snapshot
}
