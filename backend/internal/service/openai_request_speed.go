package service

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type requestSpeedContextKey struct{}
type requestSpeedSelection struct {
	groupID int64
	tier    string
}

// ErrRequestSpeedContinuationMismatch prevents resuming a supplier-owned
// response through a different tier's credentials.
var ErrRequestSpeedContinuationMismatch = errors.New("the previous response belongs to a different request tier; start a new conversation")

// WithRequestSpeed freezes trusted native Key configuration for one request or
// WS connection. Client payloads never set or override this selection.
func WithRequestSpeed(ctx context.Context, groupID int64, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestSpeedContextKey{}, requestSpeedSelection{groupID: groupID, tier: tier})
}
func RequestSpeedFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	selection, _ := ctx.Value(requestSpeedContextKey{}).(requestSpeedSelection)
	return selection.tier
}
func RequestSpeedGroupIDFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	selection, _ := ctx.Value(requestSpeedContextKey{}).(requestSpeedSelection)
	return selection.groupID
}
func RequestSpeedServiceTierFromContext(ctx context.Context) string {
	switch RequestSpeedFromContext(ctx) {
	case RequestSpeedFast:
		return OpenAIFastTierPriority
	case RequestSpeedUltrafast:
		return OpenAIFastTierUltrafast
	default:
		return ""
	}
}

// ResolveAPIKeyRequestSpeed preserves the pre-existing Ultrafast behavior of
// old (nil preference) Keys. New Keys persist the group default in native CRUD.
func ResolveAPIKeyRequestSpeed(key *APIKey) (string, error) {
	if key == nil || key.Group == nil || !key.Group.RequestSpeedSelectionEnabled {
		return "", nil
	}
	if key.Group.Platform != PlatformOpenAI || key.GroupID == nil || *key.GroupID != key.Group.ID || key.Group.ID <= 0 {
		return "", ErrRequestSpeedInvalid
	}
	if key.RequestSpeedTier == nil {
		return RequestSpeedUltrafast, nil
	}
	if !ValidRequestSpeedTier(*key.RequestSpeedTier) {
		return "", ErrRequestSpeedInvalid
	}
	return *key.RequestSpeedTier, nil
}

// AccountSupportsRequestSpeed uses the account's group association, not its
// global platform or a name match. Unassigned and stale/wrong-group candidates
// fail closed; ordinary groups retain their existing eligibility semantics.
func AccountSupportsRequestSpeed(ctx context.Context, account *Account, groupID *int64) bool {
	tier := RequestSpeedFromContext(ctx)
	if tier == "" {
		return true
	}
	trustedGroupID := RequestSpeedGroupIDFromContext(ctx)
	if !ValidRequestSpeedTier(tier) || trustedGroupID <= 0 || (groupID != nil && *groupID != trustedGroupID) ||
		account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	if group, _ := ctx.Value(ctxkey.Group).(*Group); group != nil && (group.ID != trustedGroupID || !group.RequestSpeedSelectionEnabled) {
		return false
	}
	member := false
	for _, id := range account.GroupIDs {
		if id == trustedGroupID {
			member = true
			break
		}
	}
	var associationTiers []string
	for _, association := range account.AccountGroups {
		if association.GroupID == trustedGroupID {
			member = true
			if current := association.Group; current != nil && (current.ID != trustedGroupID || current.Platform != PlatformOpenAI || current.Status != StatusActive || !current.RequestSpeedSelectionEnabled) {
				return false
			}
			associationTiers = association.RequestSpeedTiers
			break
		}
	}
	if !member {
		return false
	}
	tiers := associationTiers
	if account.RequestSpeedTiersByGroup != nil {
		// A populated native projection is authoritative; do not revive a stale
		// association value when the latest mapping has been cleared.
		tiers = account.RequestSpeedTiersByGroup[strconv.FormatInt(trustedGroupID, 10)]
	}
	// The supplier credential fixes one tier. Ambiguous old/corrupt projections
	// must not make one upstream Key eligible for both prices.
	return len(tiers) == 1 && tiers[0] == tier
}
func ValidateRequestSpeedModel(ctx context.Context, model string) *OpenAIFastBlockedError {
	if RequestSpeedFromContext(ctx) == "" {
		return nil
	}
	if !ValidRequestSpeedTier(RequestSpeedFromContext(ctx)) {
		return &OpenAIFastBlockedError{Message: "invalid request tier configuration"}
	}
	return validateGroupModelServiceTier(model, RequestSpeedServiceTierFromContext(ctx))
}

func StampFixedUpstreamRequestTier(ctx context.Context, result *OpenAIForwardResult) {
	if result == nil {
		return
	}
	if tier := RequestSpeedServiceTierFromContext(ctx); tier != "" {
		result.FixedUpstreamRequestTier = tier
		result.ServiceTier = &tier
	}
}

// Fixed supplier credentials determine the tier. The global policy can reject
// it but cannot silently strip it to Standard or rewrite it to another tier.
func (s *OpenAIGatewayService) resolveFixedRequestSpeed(ctx context.Context, account *Account, model string) (string, bool, *OpenAIFastBlockedError) {
	tier := RequestSpeedServiceTierFromContext(ctx)
	if !AccountSupportsRequestSpeed(ctx, account, nil) {
		return "", false, &OpenAIFastBlockedError{Message: "selected account is not assigned to this Key's request tier"}
	}
	if blocked := ValidateRequestSpeedModel(ctx, model); blocked != nil {
		return "", false, blocked
	}
	action, message := s.evaluateOpenAIFastPolicy(ctx, account, model, tier)
	if action == BetaPolicyActionBlock || action == BetaPolicyActionFilter || (action == OpenAIFastPolicyActionForcePriority && tier != OpenAIFastTierPriority) {
		if message == "" {
			message = "global service tier policy conflicts with this Key's fixed upstream tier"
		}
		return "", false, &OpenAIFastBlockedError{Message: message}
	}
	// Always remove client-controlled service_tier from the outbound payload.
	return tier, true, nil
}

// prepareFixedRequestSpeedBody enforces the immutable supplier route before
// protocol adapters can bypass the common Responses tier policy helper.
func (s *OpenAIGatewayService) prepareFixedRequestSpeedBody(ctx context.Context, account *Account, body []byte, defaultMappedModel string, requireCompact bool) ([]byte, error) {
	if RequestSpeedFromContext(ctx) == "" {
		return body, nil
	}
	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	model := resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)
	if defaultMappedModel != "" && !requireCompact && (account == nil || !account.IsOpenAIPassthroughEnabled()) {
		model = resolveMappedUpstreamModel(account, body, defaultMappedModel)
	}
	if blocked := s.validateFixedRequestSpeedAccount(ctx, account, model); blocked != nil {
		return body, blocked
	}
	if _, _, blocked := s.resolveFixedRequestSpeed(ctx, account, model); blocked != nil {
		return body, blocked
	}
	out, err := sjson.DeleteBytes(body, "service_tier")
	if err != nil {
		return body, err
	}
	// The Messages speed alias also becomes a tier override after conversion.
	return sjson.DeleteBytes(out, "speed")
}

// A trusted validator re-reads the authenticated native Key and Group before
// admission/each new WS turn. It is not serialized or controllable by clients.
type requestSpeedValidatorContextKey struct{}

func WithRequestSpeedValidator(ctx context.Context, validate func(context.Context) error) context.Context {
	return context.WithValue(ctx, requestSpeedValidatorContextKey{}, validate)
}
func ValidateRequestSpeedConfiguration(ctx context.Context) *OpenAIFastBlockedError {
	if RequestSpeedFromContext(ctx) == "" {
		return nil
	}
	validate, _ := ctx.Value(requestSpeedValidatorContextKey{}).(func(context.Context) error)
	if validate != nil {
		if err := validate(ctx); err != nil {
			return &OpenAIFastBlockedError{Message: "the Key or group request-tier configuration changed; retry with a new connection"}
		}
	}
	return nil
}
