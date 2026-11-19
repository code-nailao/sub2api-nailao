//go:build integration

package repository

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeAccountRequestSpeedAtomicMappingsAndMembership(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	c := tx.Client()
	r := newAccountRepositoryWithSQL(c, tx, nil)
	suffix := time.Now().UnixNano()
	first, err := c.Group.Create().SetName(fmt.Sprintf("speed-first-%d", suffix)).SetPlatform("openai").SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	second, err := c.Group.Create().SetName(fmt.Sprintf("speed-second-%d", suffix)).SetPlatform("openai").SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	ordinary, err := c.Group.Create().SetName(fmt.Sprintf("speed-ordinary-%d", suffix)).SetPlatform("openai").Save(ctx)
	require.NoError(t, err)
	a := &service.Account{Name: fmt.Sprintf("native-speed-%d", suffix), Platform: "openai", Type: "apikey", Status: "active", Schedulable: true, Concurrency: 2, Priority: 1, Credentials: map[string]any{"api_key": "local-fixed-fast"}, Extra: map[string]any{}}
	require.NoError(t, r.CreateWithAccountGroups(ctx, a, []service.AccountGroup{{GroupID: first.ID, Priority: 7, RequestSpeedTiers: []string{"fast"}}, {GroupID: second.ID, Priority: 9, RequestSpeedTiers: []string{"fast"}}, {GroupID: ordinary.ID, Priority: 11}}))
	loaded, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	firstID, secondID, ordinaryID := strconv.FormatInt(first.ID, 10), strconv.FormatInt(second.ID, 10), strconv.FormatInt(ordinary.ID, 10)
	require.Equal(t, map[string][]string{firstID: {"fast"}, secondID: {"fast"}}, loaded.RequestSpeedTiersByGroup)
	loaded.Name = "must not be written"
	invalid := map[string][]string{ordinaryID: {"fast"}}
	err = r.UpdateWithRequestSpeedGroups(ctx, loaded, nil, &invalid, nil, nil, nil)
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedDisabled)
	after, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.Name, after.Name, "rejected ordinary mapping cannot save native account fields")
	require.Equal(t, map[string][]string{firstID: {"fast"}, secondID: {"fast"}}, after.RequestSpeedTiersByGroup)

	// Updating native memberships without the new field preserves assignments
	// for retained groups and drops only the detached group's mapping.
	ids := []int64{second.ID, ordinary.ID}
	after.Name = "atomic saved name"
	require.NoError(t, r.UpdateWithRequestSpeedGroups(ctx, after, &ids, nil, nil, nil, nil))
	after, err = r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "atomic saved name", after.Name)
	require.Equal(t, map[string][]string{secondID: {"fast"}}, after.RequestSpeedTiersByGroup)
	require.ElementsMatch(t, ids, after.GroupIDs)
	require.Equal(t, 9, after.AccountGroups[0].Priority, "retained association priority preserved")
	var events int
	rows, err := c.QueryContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, a.ID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&events))
	require.NoError(t, rows.Close())
	require.Greater(t, events, 1)

	// Legacy BindGroups must not silently erase the fixed-upstream capability.
	require.NoError(t, r.BindGroups(ctx, a.ID, []int64{second.ID, ordinary.ID}))
	after, err = r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{secondID: {"fast"}}, after.RequestSpeedTiersByGroup)
}

func TestNativeAccountRequestSpeedPreservesOAuthOnlyAdmission(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	c := tx.Client()
	r := newAccountRepositoryWithSQL(c, tx, nil)
	suffix := time.Now().UnixNano()
	g, err := c.Group.Create().SetName(fmt.Sprintf("speed-oauth-%d", suffix)).SetPlatform("openai").SetRequireOauthOnly(true).SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	a := &service.Account{Name: fmt.Sprintf("speed-oauth-account-%d", suffix), Platform: "openai", Type: "apikey", Status: "active", Schedulable: true, Concurrency: 1, Priority: 1, Credentials: map[string]any{"api_key": "local"}, Extra: map[string]any{}}
	err = r.CreateWithAccountGroups(ctx, a, []service.AccountGroup{{GroupID: g.ID, RequestSpeedTiers: []string{"fast"}}})
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedOAuthOnly)
	require.Zero(t, a.ID, "no account row created before admission guard")
	require.NoError(t, r.Create(ctx, a))
	loaded, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	loaded.Name = "must roll back"
	ids := []int64{g.ID}
	empty := map[string][]string{}
	err = r.UpdateWithRequestSpeedGroups(ctx, loaded, &ids, &empty, nil, nil, nil)
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedOAuthOnly, "empty mapping must not bypass native OAuth-only membership rule")
	after, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.Name, after.Name)
	require.Empty(t, after.GroupIDs)
}

func TestNativeAccountRequestSpeedRejectsMultipleFixedTiersAtomically(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	c := tx.Client()
	r := newAccountRepositoryWithSQL(c, tx, nil)
	suffix := time.Now().UnixNano()
	g, err := c.Group.Create().SetName(fmt.Sprintf("speed-single-%d", suffix)).SetPlatform("openai").SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	a := &service.Account{Name: fmt.Sprintf("speed-single-account-%d", suffix), Platform: "openai", Type: "apikey", Status: "active", Schedulable: true, Concurrency: 1, Priority: 1, Credentials: map[string]any{"api_key": "local-fixed-fast"}, Extra: map[string]any{}}
	err = r.CreateWithAccountGroups(ctx, a, []service.AccountGroup{{GroupID: g.ID, RequestSpeedTiers: []string{"fast", "ultrafast"}}})
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedInvalid)
	require.Zero(t, a.ID, "ambiguous mapping must be rejected before account creation")
	require.NoError(t, r.CreateWithAccountGroups(ctx, a, []service.AccountGroup{{GroupID: g.ID, RequestSpeedTiers: []string{"fast"}}}))
	for _, tiers := range [][]string{{"fast", "ultrafast"}, {"fast", "fast"}} {
		loaded, err := r.GetByID(ctx, a.ID)
		require.NoError(t, err)
		loaded.Name = "must not be written"
		mappings := map[string][]string{strconv.FormatInt(g.ID, 10): tiers}
		err = r.UpdateWithRequestSpeedGroups(ctx, loaded, nil, &mappings, nil, nil, nil)
		require.ErrorIs(t, err, service.ErrAccountRequestSpeedInvalid)
		after, err := r.GetByID(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, a.Name, after.Name)
		require.Equal(t, map[string][]string{strconv.FormatInt(g.ID, 10): {"fast"}}, after.RequestSpeedTiersByGroup)
	}
}

func TestNativeAccountRequestSpeedCrossGroupConsistency(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	c := tx.Client()
	r := newAccountRepositoryWithSQL(c, tx, nil)
	suffix := time.Now().UnixNano()
	one, err := c.Group.Create().SetName(fmt.Sprintf("speed-consistent-one-%d", suffix)).SetPlatform("openai").SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	two, err := c.Group.Create().SetName(fmt.Sprintf("speed-consistent-two-%d", suffix)).SetPlatform("openai").SetRequestSpeedSelectionEnabled(true).Save(ctx)
	require.NoError(t, err)
	a := &service.Account{Name: fmt.Sprintf("speed-consistent-%d", suffix), Platform: "openai", Type: "apikey", Status: "active", Schedulable: true, Concurrency: 1, Priority: 1, Credentials: map[string]any{"api_key": "local-fixed-fast"}, Extra: map[string]any{}}
	require.NoError(t, r.CreateWithAccountGroups(ctx, a, []service.AccountGroup{{GroupID: one.ID, RequestSpeedTiers: []string{"fast"}}, {GroupID: two.ID, RequestSpeedTiers: []string{"fast"}}}))
	loaded, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	loaded.Name = "must not be written"
	mapID := func(id int64) string { return strconv.FormatInt(id, 10) }
	conflicting := map[string][]string{mapID(one.ID): {"fast"}, mapID(two.ID): {"ultrafast"}}
	err = r.UpdateWithRequestSpeedGroups(ctx, loaded, nil, &conflicting, nil, nil, nil)
	require.ErrorIs(t, err, service.ErrAccountRequestSpeedInvalid)
	after, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.Name, after.Name)
	require.Equal(t, map[string][]string{mapID(one.ID): {"fast"}, mapID(two.ID): {"fast"}}, after.RequestSpeedTiersByGroup)
	// Emulate a disabled group's old metadata. It cannot select an upstream
	// and must not constrain a still-enabled group's fixed credential tier.
	_, err = c.Group.UpdateOneID(two.ID).SetRequestSpeedSelectionEnabled(false).Save(ctx)
	require.NoError(t, err)
	_, err = c.ExecContext(ctx, `UPDATE account_groups SET request_speed_tiers='["ultrafast"]'::jsonb WHERE account_id=$1 AND group_id=$2`, a.ID, two.ID)
	require.NoError(t, err)
	ids := []int64{one.ID, two.ID}
	after.Name = "saved without changing disabled metadata"
	require.NoError(t, r.UpdateWithRequestSpeedGroups(ctx, after, &ids, nil, nil, nil, nil))
	after, err = r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "saved without changing disabled metadata", after.Name)
	require.Equal(t, conflicting, after.RequestSpeedTiersByGroup)
}
