package service

import infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

const (
	RequestSpeedFast      = "fast"
	RequestSpeedUltrafast = "ultrafast"
)

var (
	ErrRequestSpeedAccountConflict = infraerrors.Conflict("REQUEST_SPEED_ACCOUNT_CONFLICT", "同一上游 Key 在已开启的分组中必须使用相同档位")
	ErrRequestSpeedConflict        = infraerrors.Conflict("REQUEST_SPEED_CONFLICT", "分组或档位配置已变化，请刷新后重试")
	ErrRequestSpeedDisabled        = infraerrors.BadRequest("REQUEST_SPEED_DISABLED", "此分组未开放请求档位选择")
	ErrRequestSpeedUnavailable     = infraerrors.BadRequest("REQUEST_SPEED_UNAVAILABLE", "所选档位暂无可用上游账号")
	ErrRequestSpeedInvalid         = infraerrors.BadRequest("REQUEST_SPEED_INVALID", "请求档位必须为 Fast 或 Ultrafast")
	ErrRequestSpeedFastKeysExist   = infraerrors.Conflict("REQUEST_SPEED_FAST_KEYS_EXIST", "仍有 Key 选择 Fast，请先调整这些 Key 后再关闭")
)

func ValidRequestSpeedTier(tier string) bool {
	return tier == RequestSpeedFast || tier == RequestSpeedUltrafast
}

func NormalizeRequestSpeedTiers(tiers []string) ([]string, error) {
	// Each supplier credential is fixed to one tier within its group.
	if len(tiers) > 1 {
		return nil, ErrRequestSpeedInvalid
	}
	seen := make(map[string]bool, 2)
	for _, tier := range tiers {
		if !ValidRequestSpeedTier(tier) {
			return nil, ErrRequestSpeedInvalid
		}
		seen[tier] = true
	}
	out := make([]string, 0, 2)
	for _, tier := range []string{RequestSpeedFast, RequestSpeedUltrafast} {
		if seen[tier] {
			out = append(out, tier)
		}
	}
	return out, nil
}

func ValidateGroupRequestSpeed(group *Group) error {
	if group.DefaultRequestSpeedTier == "" {
		group.DefaultRequestSpeedTier = RequestSpeedUltrafast
	}
	if !ValidRequestSpeedTier(group.DefaultRequestSpeedTier) {
		return ErrRequestSpeedInvalid
	}
	if group.RequestSpeedSelectionEnabled && group.Platform != PlatformOpenAI {
		return infraerrors.BadRequest("REQUEST_SPEED_PLATFORM", "仅 OpenAI 分组支持按 Key 选择请求档位")
	}
	return nil
}
