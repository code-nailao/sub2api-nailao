package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const (
	GroupTierPassthrough    = "passthrough"
	GroupTierForceFast      = "force_fast"
	GroupTierForceUltrafast = "force_ultrafast"
)

// Only the published tier-capable families participate in the new group policy.
// Existing groups without an explicit policy keep their legacy behavior.
func supportsGroupUltrafastModel(model string) bool {
	return openAIModelSupportsUltrafast(model)
}

func supportsGroupFastModel(model string) bool {
	canonical := canonicalizeOpenAIModelAliasSpelling(model)
	return supportsGroupUltrafastModel(canonical) ||
		(strings.HasPrefix(canonical, "gpt-6-luna") && openai.IsGPT6SolOrLunaModelSpelling(canonical))
}

func validateGroupModelServiceTier(model, tier string) *OpenAIFastBlockedError {
	switch normalizeBillingServiceTier(tier) {
	case "fast", "priority":
		if !supportsGroupFastModel(model) {
			return &OpenAIFastBlockedError{Message: "Fast is supported only for gpt-6-astra, gpt-6.1-sol, gpt-6.2-sol, gpt-6.2-astra and gpt-6-luna in this group"}
		}
	case "ultrafast":
		if !supportsGroupUltrafastModel(model) {
			return &OpenAIFastBlockedError{Message: "Ultrafast is supported only for gpt-6-astra, gpt-6.1-sol, gpt-6.2-sol and gpt-6.2-astra in this group"}
		}
	}
	return nil
}

func validateGroupServiceTierPolicy(policy string) error {
	switch policy {
	case "", GroupTierPassthrough, GroupTierForceFast, GroupTierForceUltrafast:
		return nil
	default:
		return fmt.Errorf("invalid openai_service_tier_policy %q", policy)
	}
}

type requestedOpenAIServiceTierKey struct{}

func WithRequestedOpenAIServiceTier(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, requestedOpenAIServiceTierKey{}, strings.TrimSpace(tier))
}

func RequestedOpenAIServiceTierFromContext(ctx context.Context) *string {
	if ctx == nil {
		return nil
	}
	value, ok := ctx.Value(requestedOpenAIServiceTierKey{}).(string)
	if !ok {
		return nil
	}
	return &value
}

func (s *OpenAIGatewayService) groupForcedServiceTier(ctx context.Context, account *Account, model string) (string, *OpenAIFastBlockedError) {
	if ctx == nil || account == nil || account.Platform != PlatformOpenAI {
		return "", nil
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	if !IsGroupContextValid(group) || !groupSupportsOpenAIFast(group.Platform) {
		return "", nil
	}
	// An explicit Sources policy predates the native Ultra flag and keeps its
	// existing meaning. The native Ultra > Fast flags apply only in legacy mode;
	// otherwise adding a default-false native field must not change old groups.
	policy := group.OpenAIServiceTierPolicy
	if policy == "" {
		return openAIGroupForcedServiceTier(ctx, account, model), nil
	}
	settings := openAIFastPolicySettingsFromContext(ctx)
	if settings == nil && s.settingService != nil {
		settings, _ = s.settingService.GetOpenAIFastPolicySettings(ctx)
	}
	if policy != "" && settings != nil && settings.GroupTierPoliciesEnabled {
		switch policy {
		case GroupTierPassthrough:
			return "", nil
		case GroupTierForceFast:
			return OpenAIFastTierPriority, validateGroupModelServiceTier(model, OpenAIFastTierPriority)
		case GroupTierForceUltrafast:
			return OpenAIFastTierUltrafast, validateGroupModelServiceTier(model, OpenAIFastTierUltrafast)
		default:
			return "", &OpenAIFastBlockedError{Message: "invalid group service tier policy"}
		}
	}
	// A disabled feature must never silently serve a forced-Ultrafast group
	// as Standard. Legacy groups retain their existing force-Fast behavior.
	if policy == GroupTierForceUltrafast {
		return "", &OpenAIFastBlockedError{Message: "group service tier policies are disabled"}
	}
	if group.ForceOpenAIFast {
		return OpenAIFastTierPriority, nil
	}
	return "", nil
}

// Selection only: no prices or group multipliers are changed here.
// Used by HTTP, HTTP-to-WS forwarding, and native WS response.create frames.
func (s *OpenAIGatewayService) resolveOpenAIRequestServiceTier(ctx context.Context, account *Account, model, rawTier string) (tier string, remove bool, blocked *OpenAIFastBlockedError) {
	if RequestSpeedFromContext(ctx) != "" {
		return s.resolveFixedRequestSpeed(ctx, account, model)
	}
	// Reuse one settings read for explicit group policy and global policy.
	// Legacy groups keep their existing lookup path and query count.
	if ctx != nil && openAIFastPolicySettingsFromContext(ctx) == nil && s.settingService != nil {
		group, _ := ctx.Value(ctxkey.Group).(*Group)
		if IsGroupContextValid(group) && group.OpenAIServiceTierPolicy != "" {
			if settings, err := s.settingService.GetOpenAIFastPolicySettings(ctx); err == nil {
				ctx = withOpenAIFastPolicyContext(ctx, settings)
			}
		}
	}
	validateTier := func(tier string) *OpenAIFastBlockedError {
		if ctx != nil && account != nil && account.Platform == PlatformOpenAI {
			group, _ := ctx.Value(ctxkey.Group).(*Group)
			settings := openAIFastPolicySettingsFromContext(ctx)
			if IsGroupContextValid(group) && groupSupportsOpenAIFast(group.Platform) &&
				group.OpenAIServiceTierPolicy != "" && settings != nil && settings.GroupTierPoliciesEnabled {
				return validateGroupModelServiceTier(model, tier)
			}
		}
		return nil
	}
	forced, blocked := s.groupForcedServiceTier(ctx, account, model)
	if blocked != nil {
		return rawTier, false, blocked
	}
	if forced != "" {
		rawTier = forced
	}
	if rawTier == "" {
		if s.shouldForceOpenAIFastPriorityForMissingTier(ctx, account, model) {
			return OpenAIFastTierPriority, false, validateTier(OpenAIFastTierPriority)
		}
		return rawTier, false, nil
	}
	normalized := normalizedOpenAIServiceTierValue(rawTier)
	if normalized == "" {
		return rawTier, false, nil
	}
	if blocked := validateTier(normalized); blocked != nil {
		return rawTier, false, blocked
	}
	action, message := s.evaluateOpenAIFastPolicy(ctx, account, model, normalized)
	if forced == OpenAIFastTierUltrafast && privateGroupForcesUltrafast(ctx) && (action == BetaPolicyActionFilter || action == OpenAIFastPolicyActionForcePriority) {
		return rawTier, false, &OpenAIFastBlockedError{Message: "global service tier policy conflicts with this Ultrafast group"}
	}
	switch action {
	case BetaPolicyActionBlock:
		if message == "" {
			message = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normalized, model)
		}
		return rawTier, false, &OpenAIFastBlockedError{Message: message}
	case BetaPolicyActionFilter:
		return "", true, nil
	case OpenAIFastPolicyActionForcePriority:
		return OpenAIFastTierPriority, false, validateTier(OpenAIFastTierPriority)
	default:
		return normalized, false, nil
	}
}

func privateGroupForcesUltrafast(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	return group != nil && group.OpenAIServiceTierPolicy == GroupTierForceUltrafast
}
