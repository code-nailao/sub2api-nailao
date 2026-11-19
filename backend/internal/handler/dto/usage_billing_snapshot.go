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
	if snapshot.DeepSeekTools != nil {
		// Supplier identity and procurement prices are admin-only.
		return map[string]any{"version": snapshot.Version, "charge_kind": snapshot.ChargeKind,
			"deepseek_tools": map[string]any{"search_count": snapshot.DeepSeekTools.SearchCount, "fetch_count": snapshot.DeepSeekTools.FetchCount,
				"search_price_per_call": snapshot.DeepSeekTools.SearchPrice, "fetch_price_per_call": snapshot.DeepSeekTools.FetchPrice,
				"standalone": snapshot.DeepSeekTools.Standalone}}
	}
	// Existing ordinary token/tier receipts retain their established shape.
	return snapshot
}
