package service

import (
	"fmt"
	"strconv"
	"strings"
)

// ChannelMonitorV2DisplayOrder controls presentation only. GroupCategories can
// classify OpenAI-compatible domestic providers without changing routing or
// billing platforms. Unlisted categories/groups retain the client's defaults.
type ChannelMonitorV2DisplayOrder struct {
	Categories      []string           `json:"categories"`
	Groups          map[string][]int64 `json:"groups"`
	GroupCategories map[string]string  `json:"group_categories"`
	// Empty preserves legacy payloads; clients interpret it as separate.
	// Explicit per-group category overrides take precedence over this preference.
	ImageGrouping string `json:"image_grouping,omitempty"`
}

const channelMonitorV2MaxDisplayGroups = 10000

func channelMonitorV2DisplayCategory(value string) (string, error) {
	category := strings.ToLower(strings.TrimSpace(value))
	switch category {
	case "openai", "anthropic", "grok", "gemini", "domestic", "image", "other":
		return category, nil
	default:
		return "", fmt.Errorf("%w: unsupported display category %q", ErrChannelMonitorV2InvalidConfig, value)
	}
}

// NormalizeChannelMonitorV2DisplayOrder preserves ordering and returns an
// independent value so normalization/redaction cannot mutate cached config.
// nil means an old client omitted this setting; an empty object resets it.
func NormalizeChannelMonitorV2DisplayOrder(input *ChannelMonitorV2DisplayOrder) (*ChannelMonitorV2DisplayOrder, error) {
	if input == nil {
		return nil, nil
	}
	if len(input.Categories) > 100 || len(input.Groups) > 7 || len(input.GroupCategories) > channelMonitorV2MaxDisplayGroups {
		return nil, fmt.Errorf("%w: display_order is too large", ErrChannelMonitorV2InvalidConfig)
	}
	imageGrouping := strings.ToLower(strings.TrimSpace(input.ImageGrouping))
	if imageGrouping != "" && imageGrouping != "separate" && imageGrouping != "provider" {
		return nil, fmt.Errorf("%w: image_grouping must be separate or provider", ErrChannelMonitorV2InvalidConfig)
	}
	output := &ChannelMonitorV2DisplayOrder{
		Categories:      []string{},
		Groups:          map[string][]int64{},
		GroupCategories: map[string]string{},
		ImageGrouping:   imageGrouping,
	}
	seenCategories := map[string]bool{}
	for _, value := range input.Categories {
		category, err := channelMonitorV2DisplayCategory(value)
		if err != nil {
			return nil, err
		}
		if !seenCategories[category] {
			output.Categories = append(output.Categories, category)
			seenCategories[category] = true
		}
	}
	groupCount := 0
	for value, ids := range input.Groups {
		category, err := channelMonitorV2DisplayCategory(value)
		if err != nil {
			return nil, err
		}
		if _, exists := output.Groups[category]; exists {
			return nil, fmt.Errorf("%w: duplicate display category %q", ErrChannelMonitorV2InvalidConfig, category)
		}
		groupCount += len(ids)
		if groupCount > channelMonitorV2MaxDisplayGroups {
			return nil, fmt.Errorf("%w: too many ordered display groups", ErrChannelMonitorV2InvalidConfig)
		}
		output.Groups[category] = []int64{}
		seen := map[int64]bool{}
		for _, id := range ids {
			if id <= 0 {
				return nil, fmt.Errorf("%w: display group IDs must be positive", ErrChannelMonitorV2InvalidConfig)
			}
			if !seen[id] {
				output.Groups[category] = append(output.Groups[category], id)
				seen[id] = true
			}
		}
	}
	for idText, value := range input.GroupCategories {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
			return nil, fmt.Errorf("%w: display group_categories keys must be positive decimal group IDs", ErrChannelMonitorV2InvalidConfig)
		}
		category, err := channelMonitorV2DisplayCategory(value)
		if err != nil {
			return nil, err
		}
		output.GroupCategories[idText] = category
	}
	return output, nil
}

// Public layout metadata must not enumerate private groups. The configured
// scope and the authenticated user's allowed scope both apply; a missing user
// scope fails closed. Current UI filter choices do not change the saved order.
func channelMonitorV2DisplayOrderForViewer(input *ChannelMonitorV2DisplayOrder, filter ChannelMonitorV2Filter, configuredIDs []int64) *ChannelMonitorV2DisplayOrder {
	if input == nil {
		return nil
	}
	output := &ChannelMonitorV2DisplayOrder{
		Categories:      append([]string{}, input.Categories...),
		Groups:          map[string][]int64{},
		GroupCategories: map[string]string{},
		ImageGrouping:   input.ImageGrouping,
	}
	allowed := make(map[int64]bool, len(filter.AllowedGroupIDs))
	if filter.RestrictGroups {
		for _, id := range filter.AllowedGroupIDs {
			allowed[id] = true
		}
	}
	if len(configuredIDs) > 0 {
		configured := make(map[int64]bool, len(configuredIDs))
		for _, id := range configuredIDs {
			configured[id] = true
		}
		for id := range allowed {
			if !configured[id] {
				delete(allowed, id)
			}
		}
	}
	for category, ids := range input.Groups {
		visible := []int64{}
		for _, id := range ids {
			if allowed[id] {
				visible = append(visible, id)
			}
		}
		if len(visible) > 0 {
			output.Groups[category] = visible
		}
	}
	for idText, category := range input.GroupCategories {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err == nil && allowed[id] {
			output.GroupCategories[idText] = category
		}
	}
	return output
}
