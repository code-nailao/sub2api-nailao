package service

import (
	"math"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// DefaultUpstreamRejectionPrice is a final wallet amount, never a model base price.
// New groups keep billing disabled until an administrator explicitly enables it.
const DefaultUpstreamRejectionPrice = 0.1

// ValidateGroupUpstreamRejectionBillingConfig limits v1 to Grok balance groups.
// Validate the final merged group instead of silently clearing a monetary policy
// when an administrator changes the platform or subscription type.
func ValidateGroupUpstreamRejectionBillingConfig(platform, subscriptionType string, enabled bool, price float64) error {
	if err := ValidateUpstreamRejectionBillingConfig(enabled, price); err != nil {
		return err
	}
	if enabled && (platform != PlatformGrok || subscriptionType != SubscriptionTypeStandard) {
		return infraerrors.BadRequest("UNSUPPORTED_UPSTREAM_REJECTION_BILLING_GROUP", "upstream rejection billing requires a Grok standard group; explicitly disable it before changing platform or subscription type")
	}
	return nil
}

// ValidateUpstreamRejectionBillingConfig validates the merged configuration so
// partial updates cannot enable an invalid stored price. Do not round prices:
// silently rounding a configured charge could disagree with the disclosed price.
func ValidateUpstreamRejectionBillingConfig(enabled bool, price float64) error {
	if math.IsNaN(price) || math.IsInf(price, 0) || price < 0 || price >= 1e12 {
		return infraerrors.BadRequest("INVALID_UPSTREAM_REJECTION_PRICE", "upstream_rejection_price must be finite, nonnegative and less than 1000000000000")
	}
	decimal := strconv.FormatFloat(price, 'f', -1, 64)
	if point := strings.IndexByte(decimal, '.'); point >= 0 && len(decimal)-point-1 > 8 {
		return infraerrors.BadRequest("INVALID_UPSTREAM_REJECTION_PRICE", "upstream_rejection_price must have at most 8 decimal places")
	}
	if enabled && price <= 0 {
		return infraerrors.BadRequest("INVALID_UPSTREAM_REJECTION_PRICE", "upstream_rejection_price must be greater than zero when rejection billing is enabled")
	}
	return nil
}
