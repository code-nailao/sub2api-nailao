//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupUpstreamRejectionBillingPersistenceAndAuthProjection(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newGroupRepositoryWithSQL(tx.Client(), tx)
	// ORM defaults and SQL migration defaults agree for legacy creators.
	defaults, err := tx.Client().Group.Create().SetName("rejection-defaults").Save(ctx)
	require.NoError(t, err)
	require.False(t, defaults.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.1, defaults.UpstreamRejectionPrice)
	rows, err := tx.QueryContext(ctx, "INSERT INTO groups (name) VALUES ('rejection-sql-defaults') RETURNING upstream_rejection_billing_enabled, upstream_rejection_price")
	require.NoError(t, err)
	require.True(t, rows.Next())
	var sqlEnabled bool
	var sqlPrice float64
	require.NoError(t, rows.Scan(&sqlEnabled, &sqlPrice))
	require.NoError(t, rows.Close())
	require.False(t, sqlEnabled)
	require.Equal(t, 0.1, sqlPrice)

	group := &service.Group{Name: "rejection-roundtrip", Platform: service.PlatformGrok, RateMultiplier: 0.2,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: 0.12345678}
	require.NoError(t, repo.Create(ctx, group))
	stored, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, stored.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.12345678, stored.UpstreamRejectionPrice)

	user := mustCreateUser(t, tx.Client(), &service.User{})
	key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "rejection-projection-fixture", Name: "projection", Status: service.StatusActive}
	keys := NewAPIKeyRepository(tx.Client(), integrationDB)
	require.NoError(t, keys.Create(ctx, key))
	auth, err := keys.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.True(t, auth.Group.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.12345678, auth.Group.UpstreamRejectionPrice)
	require.Equal(t, 0.2, auth.Group.RateMultiplier)

	stored.UpstreamRejectionBillingEnabled = false
	require.NoError(t, repo.Update(ctx, stored))
	auth, err = keys.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.False(t, auth.Group.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.12345678, auth.Group.UpstreamRejectionPrice)

	// SQL-only updates must enqueue durable invalidation, including price-only edits.
	sum := sha256.Sum256([]byte(key.Key))
	cacheKey := hex.EncodeToString(sum[:])
	for _, update := range []string{
		"upstream_rejection_billing_enabled = true",
		"upstream_rejection_price = 0.2",
		"upstream_rejection_billing_enabled = false",
	} {
		_, err = tx.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = $1", cacheKey)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "UPDATE groups SET "+update+" WHERE id = $1", group.ID)
		require.NoError(t, err)
		rows, err := tx.QueryContext(ctx, "SELECT COUNT(*) FROM auth_cache_invalidation_outbox WHERE cache_key = $1", cacheKey)
		require.NoError(t, err)
		require.True(t, rows.Next())
		var count int
		require.NoError(t, rows.Scan(&count))
		require.NoError(t, rows.Close())
		require.Equal(t, 1, count, update)
	}
}

func TestGroupUpstreamRejectionBillingDatabaseGuards(t *testing.T) {
	for _, update := range []string{
		"upstream_rejection_price = -0.1",
		"upstream_rejection_price = 'NaN'::numeric",
		"upstream_rejection_price = 0, upstream_rejection_billing_enabled = true",
		"upstream_rejection_billing_enabled = true, platform = 'openai'",
		"upstream_rejection_billing_enabled = true, platform = 'grok', subscription_type = 'subscription'",
	} {
		t.Run(update, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			group, err := tx.Client().Group.Create().SetName("rejection-db-guard").Save(ctx)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "UPDATE groups SET "+update+" WHERE id = $1", group.ID)
			require.Error(t, err)
		})
	}
}
