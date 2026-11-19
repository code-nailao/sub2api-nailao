package service

import (
	"log/slog"
	"strings"
)

// ServiceTierBillingResolution describes how the billable service tier of one
// request was settled between the tier the client asked for and the tier the
// upstream reports having used.
type ServiceTierBillingResolution struct {
	Requested  string // tier carried by the request sent upstream ("" when none)
	Observed   string // tier declared by the upstream response ("" when none)
	Billing    string // tier used for billing and the usage log
	Downgraded bool   // Billing is cheaper than Requested
	// FixedUpstreamTier identifies a supplier credential with a fixed service
	// level. No service_tier parameter was forwarded for this route.
	FixedUpstreamTier string
}

// ResolveBillingServiceTier picks the tier to bill. The upstream declaration is
// trusted only to lower the bill: a request served below the tier it asked for
// (OpenAI "priority" answered with service_tier "default", Anthropic speed=fast
// answered with usage.speed "standard") is billed at the cheaper tier. A response
// that claims a more expensive tier, an unknown one, or none at all leaves the
// requested tier in place, which is also the behaviour for upstreams that never
// declare a tier.
func ResolveBillingServiceTier(requested, observed string) ServiceTierBillingResolution {
	requested = normalizeBillingServiceTier(requested)
	observed = normalizeBillingServiceTier(observed)
	resolution := ServiceTierBillingResolution{Requested: requested, Observed: observed, Billing: requested}
	if observed == "" || observed == requested {
		return resolution
	}
	observedRank, known := serviceTierCostRank(observed)
	if !known {
		return resolution
	}
	requestedRank, _ := serviceTierCostRank(requested)
	if observedRank >= requestedRank {
		return resolution
	}
	resolution.Billing = observed
	resolution.Downgraded = true
	return resolution
}

// serviceTierCostRank orders tiers by their cost relative to the base rate, so a
// lower rank is always cheaper. Unknown tiers rank as the base rate and report
// known=false so callers can refuse to act on them.
func serviceTierCostRank(tier string) (rank int, known bool) {
	switch normalizeBillingServiceTier(tier) {
	case "flex":
		return 0, true
	case "", "default", "standard", "auto", "scale":
		return 1, true
	case "priority", "fast":
		return 2, true
	case "ultrafast":
		return 3, true
	default:
		return 1, false
	}
}

// ResolveOpenAIServiceTierBilling applies the response-tier contract for the
// selected credential. Public OpenAI API responses declare the actual tier and
// may lower billing. The private ChatGPT Codex endpoint does not: it commonly
// reports default for effective Fast turns, so OAuth-like credentials retain
// the final outbound tier while still exposing the observed value.
func ResolveOpenAIServiceTierBilling(account *Account, requested, observed string) ServiceTierBillingResolution {
	if account != nil && account.IsOpenAIOAuthLike() && codexOAuthResponseTierIsNonAuthoritative(observed) {
		return ServiceTierBillingResolution{
			Requested: normalizeBillingServiceTier(requested),
			Observed:  normalizeBillingServiceTier(observed),
			Billing:   normalizeBillingServiceTier(requested),
		}
	}
	return ResolveBillingServiceTier(requested, observed)
}

func codexOAuthResponseTierIsNonAuthoritative(observed string) bool {
	switch normalizeBillingServiceTier(observed) {
	case "default":
		return true
	default:
		return false
	}
}

// ApplyOpenAIServiceTierBillingResolution lowers result.ServiceTier only when
// the selected credential's upstream response tier is authoritative.
func ApplyOpenAIServiceTierBillingResolution(account *Account, result *OpenAIForwardResult) ServiceTierBillingResolution {
	if result == nil {
		return ServiceTierBillingResolution{}
	}
	if account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey {
		if resolution, ok := fixedUpstreamServiceTierBilling(result.FixedUpstreamRequestTier, result.UpstreamResponseServiceTier); ok {
			tier := resolution.Billing
			result.ServiceTier = &tier
			return resolution
		}
	}
	resolution := ResolveOpenAIServiceTierBilling(account, optionalStringValue(result.ServiceTier), result.UpstreamResponseServiceTier)
	if resolution.Downgraded {
		billing := resolution.Billing
		result.ServiceTier = &billing
	}
	return resolution
}

// ApplyForwardServiceTierBillingResolution is the ForwardResult counterpart of
// ApplyOpenAIServiceTierBillingResolution.
func ApplyForwardServiceTierBillingResolution(result *ForwardResult) ServiceTierBillingResolution {
	if result == nil {
		return ServiceTierBillingResolution{}
	}
	if resolution, ok := fixedUpstreamServiceTierBilling(result.FixedUpstreamRequestTier, result.UpstreamResponseServiceTier); ok {
		tier := resolution.Billing
		result.ServiceTier = &tier
		return resolution
	}
	resolution := ResolveBillingServiceTier(optionalStringValue(result.ServiceTier), result.UpstreamResponseServiceTier)
	if resolution.Downgraded {
		billing := resolution.Billing
		result.ServiceTier = &billing
	}
	return resolution
}

// A fixed supplier Key determines the served tier, so its generic response
// service_tier is audit evidence rather than authority to change the price.
// Only canonical internal values are accepted; legacy requests keep their
// existing response-based downgrade semantics.
func fixedUpstreamServiceTierBilling(tier, observed string) (ServiceTierBillingResolution, bool) {
	if tier != OpenAIFastTierPriority && tier != OpenAIFastTierUltrafast {
		return ServiceTierBillingResolution{}, false
	}
	return ServiceTierBillingResolution{
		Observed: normalizeBillingServiceTier(observed), Billing: tier,
		FixedUpstreamTier: tier,
	}, true
}

// logServiceTierBillingDowngrade leaves an audit trail for every request billed
// below the tier it asked for; unchanged tiers are not logged.
func logServiceTierBillingDowngrade(component string, account *Account, requestID string, resolution ServiceTierBillingResolution) {
	if !resolution.Downgraded {
		return
	}
	attrs := []any{
		"component", component,
		"request_id", strings.TrimSpace(requestID),
		"requested_tier", resolution.Requested,
		"response_tier", resolution.Observed,
		"billed_tier", resolution.Billing,
	}
	if account != nil {
		attrs = append(attrs, "platform", account.Platform, "account_id", account.ID)
	}
	slog.Info("billing.service_tier_downgraded", attrs...)
}
