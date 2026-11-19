package repository

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyConditionalRenameOnlyChangesName(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	owner := mustCreateAPIKeyRepoUser(t, ctx, client, "conditional-name@example.test")
	group, err := client.Group.Create().SetName("image group").SetPlatform(service.PlatformOpenAI).Save(ctx)
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	key := &service.APIKey{UserID: owner.ID, GroupID: &group.ID, Key: "sk-name-only-test", Name: "generated", Status: service.StatusActive,
		Quota: 100, QuotaUsed: 12, ExpiresAt: &expires, RateLimit5h: 50}
	require.NoError(t, repo.Create(ctx, key))
	_, err = client.APIKey.UpdateOneID(key.ID).SetQuotaUsed(18).SetUsage5h(5).Save(ctx)
	require.NoError(t, err)
	changed, err := repo.RenameIfNameMatches(ctx, key.ID, owner.ID, group.ID, "generated", "生图 · Example")
	require.NoError(t, err)
	require.True(t, changed)
	got, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, "生图 · Example", got.Name)
	require.Equal(t, key.Key, got.Key)
	require.Equal(t, key.Status, got.Status)
	require.Equal(t, group.ID, *got.GroupID)
	require.Equal(t, 100.0, got.Quota)
	require.Equal(t, 18.0, got.QuotaUsed)
	require.Equal(t, 5.0, got.Usage5h)
	require.Equal(t, 50.0, got.RateLimit5h)
	require.True(t, expires.Equal(*got.ExpiresAt))
	for _, test := range []struct {
		owner, group int64
		name         string
	}{
		{owner.ID + 1, group.ID, got.Name}, {owner.ID, group.ID + 1, got.Name}, {owner.ID, group.ID, "generated"},
	} {
		changed, err = repo.RenameIfNameMatches(ctx, key.ID, test.owner, test.group, test.name, "must not overwrite")
		require.NoError(t, err)
		require.False(t, changed)
	}
	_, err = client.APIKey.UpdateOneID(key.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	changed, err = repo.RenameIfNameMatches(ctx, key.ID, owner.ID, group.ID, got.Name, "deleted key")
	require.NoError(t, err)
	require.False(t, changed)
}

func TestAPIKeyConditionalRenameConcurrentRequestsHaveOneWinner(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	db, ok := repo.sql.(*sql.DB)
	require.True(t, ok)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	owner := mustCreateAPIKeyRepoUser(t, ctx, client, "conditional-name-concurrent@example.test")
	group, err := client.Group.Create().SetName("image group").SetPlatform(service.PlatformOpenAI).Save(ctx)
	require.NoError(t, err)
	key := &service.APIKey{UserID: owner.ID, GroupID: &group.ID, Key: "sk-name-race-test", Name: "generated", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, name := range []string{"user name", "studio name"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			changed, err := repo.RenameIfNameMatches(ctx, key.ID, owner.ID, group.ID, "generated", name)
			results <- changed
			errs <- err
		}(name)
	}
	close(start)
	wg.Wait()
	winners := 0
	for range 2 {
		require.NoError(t, <-errs)
		if <-results {
			winners++
		}
	}
	require.Equal(t, 1, winners)
}
