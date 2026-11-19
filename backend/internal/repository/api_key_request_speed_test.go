package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKeyRequestSpeedWriteUsesLockedDefaultAndAvailability(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	groupID := int64(2)
	mock.ExpectQuery("SELECT id, platform, request_speed_selection_enabled, default_request_speed_tier").WithArgs(groupID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "enabled", "tier"}).AddRow(groupID, "openai", true, "fast"))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(groupID, "fast").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	tier, err := resolveKeyRequestSpeedForWrite(context.Background(), db, &groupID, nil, true)
	require.NoError(t, err)
	require.NotNil(t, tier)
	require.Equal(t, "fast", *tier)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyRequestSpeedWriteNeverAssignsUnavailableOrClosedTier(t *testing.T) {
	fast := "fast"
	for _, scenario := range []struct {
		name  string
		group *keyRequestSpeedGroup
		want  error
	}{
		{"unbound", nil, service.ErrRequestSpeedDisabled},
		{"ordinary", &keyRequestSpeedGroup{id: 2, enabled: false}, service.ErrRequestSpeedDisabled},
		{"no upstream", &keyRequestSpeedGroup{id: 2, enabled: true, defaultTier: "ultrafast"}, service.ErrRequestSpeedUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			if scenario.group != nil && scenario.group.enabled {
				mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(2), "fast").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			}
			_, err = validateKeyRequestSpeedForWrite(context.Background(), db, scenario.group, &fast, false)
			require.ErrorIs(t, err, scenario.want)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestKeyRequestSpeedExistingNullPreservesUltrafast(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(2), "ultrafast").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	tier, err := validateKeyRequestSpeedForWrite(context.Background(), db, &keyRequestSpeedGroup{id: 2, enabled: true, defaultTier: "fast"}, nil, false)
	require.NoError(t, err)
	require.Nil(t, tier)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyRequestSpeedGroupSwitchClearsOrdinaryPreference(t *testing.T) {
	fast := "fast"
	tier, err := validateKeyRequestSpeedForWrite(context.Background(), nil, &keyRequestSpeedGroup{id: 3, enabled: false}, &fast, true)
	require.NoError(t, err)
	require.Nil(t, tier)
	id := int64(2)
	require.True(t, nullableGroupIDEqual(sql.NullInt64{Int64: id, Valid: true}, &id))
	require.False(t, nullableGroupIDEqual(sql.NullInt64{Int64: 3, Valid: true}, &id))
	require.True(t, nullableGroupIDEqual(sql.NullInt64{}, nil))
}
