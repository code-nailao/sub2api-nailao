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

func TestNativeKeyRequestSpeedAtomicSwitchAndAuthProjection(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := newAPIKeyRepositoryWithSQL(client, tx)
	suffix := time.Now().UnixNano()
	u := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("native-speed-%d@example.com", suffix)})
	g, err := client.Group.Create().SetName(fmt.Sprintf("speed-%d", suffix)).SetPlatform("openai").
		SetStatus("active").SetSubscriptionType("standard").SetRateMultiplier(1).
		SetRequestSpeedSelectionEnabled(true).SetDefaultRequestSpeedTier("fast").Save(ctx)
	require.NoError(t, err)
	ordinary, err := client.Group.Create().SetName(fmt.Sprintf("ordinary-%d", suffix)).SetPlatform("openai").
		SetStatus("active").SetSubscriptionType("standard").SetRateMultiplier(1).Save(ctx)
	require.NoError(t, err)
	a, err := client.Account.Create().SetName("fast upstream").SetPlatform("openai").SetType("apikey").
		SetStatus("active").SetSchedulable(true).SetCredentials(map[string]any{"api_key": "local-fixture"}).
		SetConcurrency(1).SetPriority(1).Save(ctx)
	require.NoError(t, err)
	_, err = client.AccountGroup.Create().SetAccountID(a.ID).SetGroupID(g.ID).SetRequestSpeedTiers([]string{"fast"}).Save(ctx)
	require.NoError(t, err)
	k := &service.APIKey{UserID: u.ID, Key: fmt.Sprintf("sk-native-speed-%d", suffix), Name: "fast", GroupID: &g.ID, Status: "active"}
	require.NoError(t, repo.Create(ctx, k))
	require.Equal(t, "fast", *k.RequestSpeedTier)
	loaded, err := repo.GetByKeyForAuth(ctx, k.Key)
	require.NoError(t, err)
	require.Equal(t, "fast", *loaded.RequestSpeedTier)
	require.True(t, loaded.Group.RequestSpeedSelectionEnabled)
	require.Equal(t, "fast", loaded.Group.DefaultRequestSpeedTier)

	// A group switch and an unavailable explicit preference must fail together:
	// native metadata changes cannot survive a rejected speed configuration.
	k.Name = "must not save"
	k.GroupID = &ordinary.ID
	err = repo.Update(ctx, k, service.APIKeyUpdateFields{Name: true, GroupID: true, RequestSpeedTier: true, CheckExpectedGroupID: true, ExpectedGroupID: &g.ID})
	require.ErrorIs(t, err, service.ErrRequestSpeedDisabled)
	got, err := repo.GetByID(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, "fast", got.Name)
	require.Equal(t, g.ID, *got.GroupID)

	// Switching to an ordinary group clears the old speed atomically.
	k.RequestSpeedTier = nil
	require.NoError(t, repo.Update(ctx, k, service.APIKeyUpdateFields{GroupID: true, CheckExpectedGroupID: true, ExpectedGroupID: &g.ID}))
	got, err = repo.GetByID(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, ordinary.ID, *got.GroupID)
	require.Nil(t, got.RequestSpeedTier)

	// A stale tab cannot write a preference after another tab changed the group.
	fast := "fast"
	k.GroupID = &g.ID
	k.RequestSpeedTier = &fast
	err = repo.Update(ctx, k, service.APIKeyUpdateFields{RequestSpeedTier: true, CheckExpectedGroupID: true, ExpectedGroupID: &g.ID})
	require.ErrorIs(t, err, service.ErrRequestSpeedConflict)
}
