package service

import (
	"context"
	"strings"
	"time"
)

// ResolveUsageGroups returns current local API-key membership for the admin
// monitor list. It does not authenticate, touch usage, or cache associations:
// replacing a probe key or moving it to another group takes effect on refresh.
func (s *ChannelMonitorService) ResolveUsageGroups(ctx context.Context, monitors []*ChannelMonitor) (map[int64]int64, error) {
	keys := make([]string, 0, len(monitors))
	seen := make(map[string]bool)
	for _, m := range monitors {
		key := monitorUsageKey(m)
		if key != "" && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	result := make(map[int64]int64)
	if len(keys) == 0 {
		return result, nil
	}
	// Optional metadata must not hold up probe status when storage is unhealthy.
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	groups, err := s.repo.ResolveUsageGroupsByAPIKeys(lookup, keys)
	if err != nil {
		return result, err
	}
	for _, m := range monitors {
		if key := monitorUsageKey(m); key != "" {
			if group := groups[key]; group > 0 {
				result[m.ID] = group
			}
		}
	}
	return result, nil
}

// Custom authentication headers override adapter defaults. Without resolving
// the adapter's effective identity, the configured APIKey cannot prove which
// local group is probed; report unavailable instead of another group's usage.
func monitorUsageKey(m *ChannelMonitor) string {
	if m == nil || m.APIKeyDecryptFailed || m.APIKey == "" || m.CheckMode == MonitorCheckModeQuota {
		return ""
	}
	for name := range m.ExtraHeaders {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization", "proxy-authorization", "x-api-key", "x-goog-api-key", "api-key":
			return ""
		}
	}
	return m.APIKey
}
