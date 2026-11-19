//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeGroupRequestSpeedConcurrentEnableRejectsConflictingCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := integrationEntClient
	r := newGroupRepositoryWithSQL(c, integrationDB)
	suffix := time.Now().UnixNano()
	first, err := c.Group.Create().SetName(fmt.Sprintf("concurrent-speed-a-%d", suffix)).SetPlatform("openai").Save(ctx)
	require.NoError(t, err)
	second, err := c.Group.Create().SetName(fmt.Sprintf("concurrent-speed-b-%d", suffix)).SetPlatform("openai").Save(ctx)
	require.NoError(t, err)
	a, err := c.Account.Create().SetName(fmt.Sprintf("single-fixed-key-%d", suffix)).SetPlatform("openai").SetType("apikey").
		SetStatus("active").SetSchedulable(true).SetCredentials(map[string]any{}).SetConcurrency(1).SetPriority(1).Save(ctx)
	require.NoError(t, err)
	// These fixtures model retained metadata on two disabled groups. Each edge
	// respects the single-assignment constraint; enabling both would not.
	_, err = c.AccountGroup.Create().SetAccountID(a.ID).SetGroupID(first.ID).SetRequestSpeedTiers([]string{"fast"}).Save(ctx)
	require.NoError(t, err)
	_, err = c.AccountGroup.Create().SetAccountID(a.ID).SetGroupID(second.ID).SetRequestSpeedTiers([]string{"ultrafast"}).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM scheduler_outbox WHERE group_id = ANY($1) OR account_id = $2`, fmt.Sprintf("{%d,%d}", first.ID, second.ID), a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM account_groups WHERE account_id = $1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM groups WHERE id IN ($1, $2)`, first.ID, second.ID)
	})
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{first.ID, second.ID} {
		g, err := r.GetByIDLite(ctx, id)
		require.NoError(t, err)
		g.RequestSpeedSelectionEnabled = true
		wg.Add(1)
		go func(group *service.Group) { defer wg.Done(); <-start; errs <- r.Update(ctx, group) }(g)
	}
	close(start)
	wg.Wait()
	close(errs)
	passed, rejected := 0, 0
	for err := range errs {
		if err == nil {
			passed++
		} else {
			require.ErrorIs(t, err, service.ErrRequestSpeedAccountConflict)
			rejected++
		}
	}
	require.Equal(t, 1, passed)
	require.Equal(t, 1, rejected)
	var enabled int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM groups WHERE id IN ($1,$2) AND request_speed_selection_enabled`, first.ID, second.ID).Scan(&enabled))
	require.Equal(t, 1, enabled, "concurrent activation cannot put the same credential in two effective tiers")
}
