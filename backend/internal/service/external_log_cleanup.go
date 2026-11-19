package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// SettingKeyExternalLogCleanupEnabled assigns only log retention to the
// separately guarded maintenance operator. It does not disable aggregation,
// billing dedup, auditing or the other background services.
const SettingKeyExternalLogCleanupEnabled = "external_log_cleanup_enabled"

type externalCleanupOwnership struct{ observedEnabled atomic.Bool }

func (o *externalCleanupOwnership) read(ctx context.Context, repo SettingRepository) (bool, error) {
	if repo == nil {
		if o.observedEnabled.Load() {
			return true, fmt.Errorf("external cleanup ownership repository unavailable")
		}
		return false, nil
	}
	raw, err := repo.GetValue(ctx, SettingKeyExternalLogCleanupEnabled)
	if err != nil {
		return true, fmt.Errorf("external cleanup ownership unavailable: %w", err)
	}
	switch strings.TrimSpace(raw) {
	case "true":
		o.observedEnabled.Store(true)
		return true, nil
	case "false":
		o.observedEnabled.Store(false)
		return false, nil // An explicit, operator-controlled handback.
	default:
		return true, fmt.Errorf("external cleanup ownership is not a boolean")
	}
}

func externallyManagedOpsTable(table string) bool {
	switch table {
	case "ops_system_logs", "ops_error_logs", "ops_metrics_hourly", "ops_metrics_daily":
		return true
	default:
		return false
	}
}
