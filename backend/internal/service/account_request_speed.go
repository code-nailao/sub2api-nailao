package service

import (
	"context"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrAccountRequestSpeedInvalid     = infraerrors.BadRequest("REQUEST_SPEED_INVALID", "无效的账号请求档位配置")
	ErrAccountRequestSpeedGroup       = infraerrors.BadRequest("REQUEST_SPEED_ACCOUNT_GROUP", "账号必须已关联该分组")
	ErrAccountRequestSpeedDisabled    = infraerrors.BadRequest("REQUEST_SPEED_DISABLED", "此分组未开放请求档位选择")
	ErrAccountRequestSpeedUnavailable = infraerrors.ServiceUnavailable("REQUEST_SPEED_PERSISTENCE_UNAVAILABLE", "请求档位事务保存暂不可用")
	ErrAccountRequestSpeedOAuthOnly   = infraerrors.BadRequest("REQUEST_SPEED_OAUTH_ONLY", "仅允许 OAuth 的分组不能绑定固定档位 API Key 上游")
)

// RefreshRequestSpeedMappings derives the API/cache map from persisted edges.
// Copies prevent a caller mutating a shared scheduling snapshot through a slice.
func (a *Account) RefreshRequestSpeedMappings() {
	a.RequestSpeedTiersByGroup = make(map[string][]string)
	for _, edge := range a.AccountGroups {
		if len(edge.RequestSpeedTiers) > 0 {
			a.RequestSpeedTiersByGroup[strconv.FormatInt(edge.GroupID, 10)] = append([]string(nil), edge.RequestSpeedTiers...)
		}
	}
}

// NormalizeAccountRequestSpeedMappings validates only explicitly submitted
// mappings. A nil update map means preserve existing assignments.
func NormalizeAccountRequestSpeedMappings(ctx context.Context, groups GroupRepository, platform, accountType string, groupIDs []int64, mappings map[string][]string) (map[string][]string, error) {
	clean := make(map[string][]string)
	fixedTier := ""
	if len(mappings) > 200 {
		return nil, ErrAccountRequestSpeedInvalid
	}
	bound := make(map[int64]bool, len(groupIDs))
	for _, id := range groupIDs {
		bound[id] = true
	}
	for rawID, tiers := range mappings {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rawID || !bound[id] {
			return nil, ErrAccountRequestSpeedGroup
		}
		// A supplier credential is configured for exactly one fixed tier.
		if len(tiers) > 1 {
			return nil, ErrAccountRequestSpeedInvalid
		}
		seen := map[string]bool{}
		for _, tier := range tiers {
			if tier != "fast" && tier != "ultrafast" {
				return nil, ErrAccountRequestSpeedInvalid
			}
			seen[tier] = true
		}
		if len(seen) == 0 {
			continue
		}
		if platform != PlatformOpenAI || accountType != AccountTypeAPIKey {
			return nil, ErrAccountRequestSpeedInvalid
		}
		g, err := groups.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if g.RequireOAuthOnly {
			return nil, ErrAccountRequestSpeedOAuthOnly
		}
		if g.Platform != PlatformOpenAI || !g.RequestSpeedSelectionEnabled {
			return nil, ErrAccountRequestSpeedDisabled
		}
		for _, tier := range []string{"fast", "ultrafast"} {
			if seen[tier] {
				if fixedTier != "" && fixedTier != tier {
					return nil, ErrAccountRequestSpeedInvalid
				}
				fixedTier = tier
				clean[rawID] = append(clean[rawID], tier)
			}
		}
	}
	return clean, nil
}

func accountGroupsWithRequestSpeed(groupIDs []int64, mappings map[string][]string) []AccountGroup {
	groups := make([]AccountGroup, 0, len(groupIDs))
	for i, id := range groupIDs {
		groups = append(groups, AccountGroup{GroupID: id, Priority: i + 1, RequestSpeedTiers: append([]string(nil), mappings[strconv.FormatInt(id, 10)]...)})
	}
	return groups
}
