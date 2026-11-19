package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func monitorDisplayConfigRows(version int, displayOrder ...string) *sqlmock.Rows {
	columns := []string{"version", "enabled", "refresh_interval_seconds", "platforms", "group_ids", "ignored_error_categories", "health_thresholds", "updated_at", "updated_by"}
	if len(displayOrder) > 0 {
		columns = append(columns, "display_order")
		return sqlmock.NewRows(columns).AddRow(version, true, 60, `[ {"platform":"openai","enabled":true,"models":[]} ]`, "{216,224}", "{}", "{}", time.Unix(1794846000, 0), 1, displayOrder[0])
	}
	return sqlmock.NewRows(columns).AddRow(version, true, 60, `[ {"platform":"openai","enabled":true,"models":[]} ]`, "{216,224}", "{}", "{}", time.Unix(1794846000, 0), 1)
}

func TestChannelMonitorV2ConfigLoadsOptionalDisplayOrder(t *testing.T) {
	for _, raw := range []string{"null", `{"categories":["domestic","openai"],"groups":{"domestic":[224,216]},"group_categories":{"216":"domestic"},"future_field":true}`, "malformed"} {
		t.Run(raw, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery("SELECT version, enabled").WithArgs(channelMonitorV2DisplayOrderSettingKey).WillReturnRows(monitorDisplayConfigRows(3, raw))
			cfg, err := (&channelMonitorV2Repository{db: db}).GetConfig(context.Background())
			require.NoError(t, err)
			require.Equal(t, 3, cfg.Version)
			if raw == "null" || raw == "malformed" {
				require.Nil(t, cfg.DisplayOrder)
			} else {
				require.Equal(t, []int64{224, 216}, cfg.DisplayOrder.Groups["domestic"])
				require.Equal(t, "domestic", cfg.DisplayOrder.GroupCategories["216"])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2DisplayOrderUpdateAndLegacyPreservation(t *testing.T) {
	stored := `{"categories":["domestic","openai"],"groups":{"domestic":[224,216]},"group_categories":{"216":"domestic","224":"domestic"}}`
	for _, supplied := range []bool{false, true} {
		name := "old client preserves preferences"
		if supplied {
			name = "new client persists explicit reset"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			cfg := service.ChannelMonitorV2Config{Version: 3, Enabled: true, RefreshIntervalSeconds: 60}
			if supplied {
				cfg.DisplayOrder = &service.ChannelMonitorV2DisplayOrder{Categories: []string{}, Groups: map[string][]int64{}, GroupCategories: map[string]string{}}
			}
			mock.ExpectBegin()
			mock.ExpectQuery("UPDATE channel_monitor_v2_config").WithArgs(true, 60, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 3).WillReturnRows(monitorDisplayConfigRows(4))
			resultRaw := stored
			if supplied {
				raw, err := json.Marshal(cfg.DisplayOrder)
				require.NoError(t, err)
				resultRaw = string(raw)
				mock.ExpectExec("INSERT INTO settings").WithArgs(channelMonitorV2DisplayOrderSettingKey, resultRaw).WillReturnResult(sqlmock.NewResult(1, 1))
			}
			mock.ExpectQuery("SELECT COALESCE").WithArgs(channelMonitorV2DisplayOrderSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(resultRaw))
			mock.ExpectCommit()
			updated, err := (&channelMonitorV2Repository{db: db}).UpdateConfig(context.Background(), cfg, 3)
			require.NoError(t, err)
			require.Equal(t, 4, updated.Version)
			require.NotNil(t, updated.DisplayOrder)
			if supplied {
				require.Empty(t, updated.DisplayOrder.Groups)
			} else {
				require.Equal(t, []int64{224, 216}, updated.DisplayOrder.Groups["domestic"])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2DisplayOrderConflictDoesNotWritePreferences(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE channel_monitor_v2_config").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	updated, err := (&channelMonitorV2Repository{db: db}).UpdateConfig(context.Background(), service.ChannelMonitorV2Config{DisplayOrder: &service.ChannelMonitorV2DisplayOrder{}}, 1)
	require.ErrorIs(t, err, service.ErrChannelMonitorV2ConfigConflict)
	require.Nil(t, updated)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorV2DisplayOrderSaveFailureRollsBackConfigVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE channel_monitor_v2_config").WillReturnRows(monitorDisplayConfigRows(4))
	saveErr := errors.New("display preference store unavailable")
	mock.ExpectExec("INSERT INTO settings").WillReturnError(saveErr)
	mock.ExpectRollback()
	updated, err := (&channelMonitorV2Repository{db: db}).UpdateConfig(context.Background(), service.ChannelMonitorV2Config{DisplayOrder: &service.ChannelMonitorV2DisplayOrder{}}, 3)
	require.ErrorIs(t, err, saveErr)
	require.Nil(t, updated)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorV2MetricAccumulatorExposesPresenceToAllViewers(t *testing.T) {
	for _, admin := range []bool{false, true} {
		empty := newMetricAccumulator().metric(5, admin)
		require.False(t, empty.HasRequests)
		require.False(t, empty.HasCacheInput)
		acc := newMetricAccumulator()
		acc.addFact(channelMonitorV2Fact{Success: 1, Input: 100})
		metric := acc.metric(5, admin)
		require.True(t, metric.HasRequests)
		require.True(t, metric.HasCacheInput)
		require.Zero(t, metric.CacheRate, "no cache hits with input is a valid zero, not missing data")
	}
}
