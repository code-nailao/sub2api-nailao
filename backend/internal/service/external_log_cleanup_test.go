package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestExternalLogCleanupOwnershipFailsClosed(t *testing.T) {
	r := newRuntimeSettingRepoStub()
	r.values[SettingKeyExternalLogCleanupEnabled] = "false"
	o := &externalCleanupOwnership{}
	ctx := context.Background()
	owned, err := o.read(ctx, r)
	require.NoError(t, err)
	require.False(t, owned)
	r.values[SettingKeyExternalLogCleanupEnabled] = "true"
	owned, err = o.read(ctx, r)
	require.NoError(t, err)
	require.True(t, owned)
	delete(r.values, SettingKeyExternalLogCleanupEnabled)
	owned, err = o.read(ctx, r)
	require.Error(t, err)
	require.True(t, owned)
	r.values[SettingKeyExternalLogCleanupEnabled] = "false"
	owned, err = o.read(ctx, r)
	require.NoError(t, err)
	require.False(t, owned)
	r.getValueFn = func(string) (string, error) { return "", errors.New("unavailable") }
	owned, err = o.read(ctx, r)
	require.Error(t, err)
	require.True(t, owned)
}

func TestExternalLogCleanupPreservesDedupAndNonLogTargets(t *testing.T) {
	settings := newRuntimeSettingRepoStub()
	settings.values[SettingKeyExternalLogCleanupEnabled] = "true"
	settings.values[SettingKeyOpsRuntimeLogConfig] = `{"request_retention_days":90}`
	repo := &dashboardAggregationRepoTestStub{}
	svc := NewDashboardAggregationService(repo, nil, &config.Config{DashboardAgg: config.DashboardAggregationConfig{
		Enabled: true, Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 90, UsageBillingDedupDays: 365},
	}})
	svc.settingRepo = settings
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	svc.maybeCleanupRetention(context.Background(), now)
	require.Zero(t, repo.cleanupAggregateCalls)
	require.Zero(t, repo.cleanupUsageCalls)
	require.Equal(t, 1, repo.cleanupDedupCalls)
	require.Equal(t, now.AddDate(0, 0, -365), repo.cleanupDedupCutoff)
	for _, name := range []string{"ops_error_logs", "ops_system_logs", "ops_metrics_hourly", "ops_metrics_daily"} {
		require.True(t, externallyManagedOpsTable(name))
	}
	for _, name := range []string{"ops_system_log_cleanup_audits", "ops_system_metrics", "ops_alert_events", "ops_ingress_reject_aggregates"} {
		require.False(t, externallyManagedOpsTable(name))
	}
}

func TestExternalLogCleanupRefusesManualAndQueuedTasks(t *testing.T) {
	settings := newRuntimeSettingRepoStub()
	settings.values[SettingKeyExternalLogCleanupEnabled] = "true"
	repo := &cleanupRepoStub{claimQueue: []*UsageCleanupTask{{ID: 1, Status: UsageCleanupStatusPending}}}
	dashboard := &DashboardAggregationService{settingRepo: settings}
	svc := NewUsageCleanupService(repo, nil, dashboard, nil)
	_, err := svc.CreateTask(context.Background(), UsageCleanupFilters{}, 1)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	require.Equal(t, "EXTERNAL_LOG_CLEANUP_MANAGED", infraerrors.Reason(err))
	require.Empty(t, repo.created)
	svc.runOnce()
	require.Len(t, repo.claimQueue, 1)
	require.Empty(t, repo.deleteCalls)
}

func TestExternalLogCleanupOpsExecutesOnlyUnmanagedTargets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	settings := newRuntimeSettingRepoStub()
	settings.values[SettingKeyExternalLogCleanupEnabled] = "true"
	svc := &OpsCleanupService{db: db, cfg: &config.Config{}, settingRepo: settings,
		effective: opsCleanupEffectiveConfig{SystemLogRetentionDays: 30, OpsCleanupConfig: config.OpsCleanupConfig{
			ErrorLogRetentionDays: 30, MinuteMetricsRetentionDays: 30, HourlyMetricsRetentionDays: 30,
		}}}
	for _, table := range []string{"ops_ingress_reject_aggregates", "ops_alert_events", "ops_system_log_cleanup_audits", "ops_system_metrics"} {
		mock.ExpectExec("(?s)WITH batch AS .*FROM "+regexp.QuoteMeta(table)+".*DELETE FROM "+regexp.QuoteMeta(table)).
			WithArgs(sqlmock.AnyArg(), 5000).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	_, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
