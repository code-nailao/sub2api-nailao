package repository

import (
	"context"
	"encoding/json"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRequestSpeedCreateRejectsDisabledBeforeNativeWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	r := &accountRepository{client: client, sql: client}
	mock.ExpectBegin()
	mock.ExpectExec("sources_request_speed_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("account_group_live_group_lock").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(2))
	mock.ExpectQuery(`SELECT .* FROM "groups"`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "request_speed_selection_enabled"}).AddRow(2, service.PlatformOpenAI, false))
	mock.ExpectRollback()
	err = r.CreateWithAccountGroups(context.Background(), &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, []service.AccountGroup{{GroupID: 2, RequestSpeedTiers: []string{"fast"}}})
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedDisabled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRequestSpeedUpdateRejectsDisabledBeforeNativeWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	r := &accountRepository{client: client, sql: client}
	mock.ExpectBegin()
	mock.ExpectExec("sources_request_speed_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .* FROM "account_groups"`).WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "priority", "request_speed_tiers"}).AddRow(8, 2, 1, `[]`))
	mock.ExpectQuery("account_group_live_group_lock").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(2))
	mock.ExpectQuery(`SELECT .* FROM "groups"`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "request_speed_selection_enabled"}).AddRow(2, service.PlatformOpenAI, false))
	mock.ExpectRollback()
	mappings := map[string][]string{"2": {"fast"}}
	err = r.UpdateWithRequestSpeedGroups(context.Background(), &service.Account{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, nil, &mappings, nil, nil, nil)
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedDisabled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRequestSpeedSchedulerMetadataRoundtrip(t *testing.T) {
	a := service.Account{ID: 8, GroupIDs: []int64{1, 2}, AccountGroups: []service.AccountGroup{{GroupID: 1, RequestSpeedTiers: []string{"fast"}}, {GroupID: 2, RequestSpeedTiers: []string{"ultrafast"}}}}
	a.RefreshRequestSpeedMappings()
	_, raw, err := marshalSchedulerCacheAccount(a)
	require.NoError(t, err)
	var decoded service.Account
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, a.RequestSpeedTiersByGroup, decoded.RequestSpeedTiersByGroup)
	require.Equal(t, a.AccountGroups[0].RequestSpeedTiers, decoded.AccountGroups[0].RequestSpeedTiers)
	meta := buildSchedulerMetadataAccount(a)
	meta.RequestSpeedTiersByGroup["1"][0] = "ultrafast"
	meta.AccountGroups[1].RequestSpeedTiers[0] = "fast"
	require.Equal(t, "fast", a.RequestSpeedTiersByGroup["1"][0])
	require.Equal(t, "ultrafast", a.AccountGroups[1].RequestSpeedTiers[0])
}

func TestAccountRequestSpeedLockedValidationRejectsMultipleFixedTiers(t *testing.T) {
	for _, validateAssignments := range []bool{true, false} {
		for _, tiers := range [][]string{{"fast", "ultrafast"}, {"fast", "fast"}} {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectQuery(`SELECT .* FROM "groups"`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "request_speed_selection_enabled"}).AddRow(2, service.PlatformOpenAI, true))
			err = validateLockedAccountRequestSpeeds(context.Background(), client, &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, []service.AccountGroup{{GroupID: 2, RequestSpeedTiers: tiers}}, validateAssignments)
			require.ErrorIs(t, err, service.ErrAccountRequestSpeedInvalid, "must also reject ambiguous retained mappings on membership-only writes")
			require.NoError(t, mock.ExpectationsWereMet())
			_ = db.Close()
		}
	}
}

func TestAccountRequestSpeedLockedValidationChecksEnabledGroupConsistency(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		secondEnabled, validateAssignments bool
		secondTier                         string
		want                               error
	}{
		{"new conflicting mapping", true, true, "ultrafast", service.ErrAccountRequestSpeedInvalid},
		{"retained conflicting mapping", true, false, "ultrafast", service.ErrAccountRequestSpeedInvalid},
		{"same supplier tier reused", true, false, "fast", nil},
		{"disabled legacy mapping ignored", false, false, "ultrafast", nil},
		{"disabled submitted mapping rejected", false, true, "ultrafast", service.ErrAccountRequestSpeedDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectQuery(`SELECT .* FROM "groups"`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "request_speed_selection_enabled"}).AddRow(1, service.PlatformOpenAI, true))
			mock.ExpectQuery(`SELECT .* FROM "groups"`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "request_speed_selection_enabled"}).AddRow(2, service.PlatformOpenAI, tc.secondEnabled))
			err = validateLockedAccountRequestSpeeds(context.Background(), client, &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, []service.AccountGroup{{GroupID: 1, RequestSpeedTiers: []string{"fast"}}, {GroupID: 2, RequestSpeedTiers: []string{tc.secondTier}}}, tc.validateAssignments)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
