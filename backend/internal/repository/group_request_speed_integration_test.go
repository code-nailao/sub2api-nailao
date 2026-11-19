//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeGroupRequestSpeedAvailabilityAndDisableGuard(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	c := tx.Client()
	r := newGroupRepositoryWithSQL(c, tx)
	suffix := time.Now().UnixNano()
	g := &service.Group{Name: fmt.Sprintf("speed-group-%d", suffix), Platform: "openai", Status: "active",
		RateMultiplier: 1, SubscriptionType: "standard", RequestSpeedSelectionEnabled: true,
		DefaultRequestSpeedTier: "ultrafast"}
	require.NoError(t, r.Create(ctx, g))
	loaded, err := r.GetByIDLite(ctx, g.ID)
	require.NoError(t, err)
	require.True(t, loaded.RequestSpeedSelectionEnabled)
	require.Empty(t, loaded.AvailableRequestSpeedTiers, "a new enabled group must not invent upstream availability")
	a, err := c.Account.Create().SetName("fixed fast").SetPlatform("openai").SetType("apikey").
		SetStatus("active").SetSchedulable(true).SetCredentials(map[string]any{}).SetConcurrency(1).SetPriority(1).Save(ctx)
	require.NoError(t, err)
	_, err = c.AccountGroup.Create().SetAccountID(a.ID).SetGroupID(g.ID).SetRequestSpeedTiers([]string{"fast"}).Save(ctx)
	require.NoError(t, err)
	loaded, err = r.GetByIDLite(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"fast"}, loaded.AvailableRequestSpeedTiers)
	u := mustCreateUser(t, c, &service.User{Email: fmt.Sprintf("native-group-%d@example.com", suffix)})
	_, err = c.APIKey.Create().SetUserID(u.ID).SetGroupID(g.ID).SetKey(fmt.Sprintf("local-group-%d", suffix)).
		SetName("fast key").SetStatus("active").SetRequestSpeedTier("fast").Save(ctx)
	require.NoError(t, err)
	loaded.RequestSpeedSelectionEnabled = false
	loaded.Name = "must not change"
	require.ErrorIs(t, r.Update(ctx, loaded), service.ErrRequestSpeedFastKeysExist)
	after, err := r.GetByIDLite(ctx, g.ID)
	require.NoError(t, err)
	require.True(t, after.RequestSpeedSelectionEnabled)
	require.Equal(t, g.Name, after.Name, "failed disable must roll back all native fields")
}
