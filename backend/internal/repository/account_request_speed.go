package repository

import (
	"context"
	"strconv"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// validateLockedAccountRequestSpeeds repeats the business gate while group
// SHARE locks are held, so concurrently disabling a group cannot admit a new
// assignment. Empty assignments are always allowed to remove old metadata.
func validateLockedAccountRequestSpeeds(ctx context.Context, client *dbent.Client, account *service.Account, groups []service.AccountGroup, validateAssignments bool) error {
	fixedTier := ""
	for _, edge := range groups {
		g, err := client.Group.Query().Where(dbgroup.IDEQ(edge.GroupID)).Only(ctx)
		if err != nil {
			return translatePersistenceError(err, service.ErrGroupNotFound, nil)
		}
		if account.Type == service.AccountTypeAPIKey && g.RequireOauthOnly && (g.Platform == service.PlatformOpenAI || g.Platform == service.PlatformAntigravity || g.Platform == service.PlatformAnthropic || g.Platform == service.PlatformGemini || g.Platform == service.PlatformGrok) {
			return service.ErrAccountRequestSpeedOAuthOnly
		}
		// Even membership-only edits must not retain an ambiguous credential.
		if len(edge.RequestSpeedTiers) > 1 {
			return service.ErrAccountRequestSpeedInvalid
		}
		if len(edge.RequestSpeedTiers) == 0 {
			continue
		}
		if account.Platform != service.PlatformOpenAI || account.Type != service.AccountTypeAPIKey {
			return service.ErrAccountRequestSpeedInvalid
		}
		if g.Platform != service.PlatformOpenAI || !g.RequestSpeedSelectionEnabled {
			if validateAssignments {
				return service.ErrAccountRequestSpeedDisabled
			}
			continue // Disabled groups cannot determine this credential's active tier.
		}
		seen := map[string]bool{}
		for _, tier := range edge.RequestSpeedTiers {
			if (tier != "fast" && tier != "ultrafast") || seen[tier] {
				return service.ErrAccountRequestSpeedInvalid
			}
			if fixedTier != "" && fixedTier != tier {
				return service.ErrAccountRequestSpeedInvalid
			}
			fixedTier = tier
			seen[tier] = true
		}
	}
	return nil
}

func (r *accountRepository) UpdateWithRequestSpeedGroups(ctx context.Context, account *service.Account, groupIDs *[]int64, mappings *map[string][]string, probeEnabled, rateSyncEnabled *bool, rateMultiplier *float64) error {
	if account == nil {
		return service.ErrAccountNilInput
	}
	baseCtx := ctx
	tx := dbent.TxFromContext(ctx)
	owned := false
	if tx == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil {
			return err
		}
		owned = true
		defer func() { _ = tx.Rollback() }()
		ctx = dbent.NewTxContext(ctx, tx)
	}
	client := tx.Client()
	if _, err := client.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sources_request_speed_config', 0))`); err != nil {
		return err
	}
	// Match the existing scoped-admin account assignment fence.
	if _, err := client.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('scoped_admin_account:' || $1::text, 0))`, account.ID); err != nil {
		return err
	}
	scoped := *r
	scoped.client, scoped.sql, scoped.schedulerCache = client, client, nil
	existing, err := client.AccountGroup.Query().Where(dbaccountgroup.AccountIDEQ(account.ID)).All(ctx)
	if err != nil {
		return err
	}
	oldIDs := make([]int64, 0, len(existing))
	byID := make(map[int64]*dbent.AccountGroup, len(existing))
	for _, edge := range existing {
		oldIDs = append(oldIDs, edge.GroupID)
		byID[edge.GroupID] = edge
	}
	nextIDs := oldIDs
	if groupIDs != nil {
		nextIDs = uniquePositiveInt64s(*groupIDs)
	}
	if err := lockLiveGroups(ctx, client, nextIDs); err != nil {
		return err
	}
	bound := map[int64]bool{}
	for _, id := range nextIDs {
		bound[id] = true
	}
	if mappings != nil {
		for rawID := range *mappings {
			id, e := strconv.ParseInt(rawID, 10, 64)
			if e != nil || !bound[id] || strconv.FormatInt(id, 10) != rawID {
				return service.ErrAccountRequestSpeedGroup
			}
		}
	}
	next := make([]service.AccountGroup, 0, len(nextIDs))
	for i, id := range nextIDs {
		edge := service.AccountGroup{AccountID: account.ID, GroupID: id, Priority: i + 1}
		if old := byID[id]; old != nil {
			edge.Priority = old.Priority
			edge.CreatedAt = old.CreatedAt
			edge.RequestSpeedTiers = append([]string(nil), old.RequestSpeedTiers...)
		}
		if mappings != nil {
			edge.RequestSpeedTiers = append([]string(nil), (*mappings)[strconv.FormatInt(id, 10)]...)
		}
		next = append(next, edge)
	}
	if err := validateLockedAccountRequestSpeeds(ctx, client, account, next, mappings != nil); err != nil {
		return err
	}
	// Native account changes and exact group assignments share this transaction.
	account.GroupIDs = append([]int64(nil), nextIDs...)
	if err := scoped.updateAccount(ctx, account, probeEnabled, rateSyncEnabled, rateMultiplier); err != nil {
		return err
	}
	removed := []int64{}
	for _, id := range oldIDs {
		if !bound[id] {
			removed = append(removed, id)
		}
	}
	if len(removed) > 0 {
		if _, err := client.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(account.ID), dbaccountgroup.GroupIDIn(removed...)).Exec(ctx); err != nil {
			return err
		}
	}
	for _, edge := range next {
		if old := byID[edge.GroupID]; old != nil {
			if mappings != nil {
				_, err := client.AccountGroup.Update().Where(dbaccountgroup.AccountIDEQ(account.ID), dbaccountgroup.GroupIDEQ(edge.GroupID)).SetRequestSpeedTiers(append([]string{}, edge.RequestSpeedTiers...)).Save(ctx)
				if err != nil {
					return err
				}
			}
		} else {
			_, err := client.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(edge.GroupID).SetPriority(edge.Priority).SetRequestSpeedTiers(append([]string{}, edge.RequestSpeedTiers...)).Save(ctx)
			if err != nil {
				return err
			}
		}
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountGroupsChanged, &account.ID, nil, buildSchedulerGroupPayload(mergeGroupIDs(oldIDs, nextIDs))); err != nil {
		return err
	}
	account.AccountGroups = next
	account.RefreshRequestSpeedMappings()
	if owned {
		if err := tx.Commit(); err != nil {
			return err
		}
		r.syncSchedulerAccountSnapshot(baseCtx, account.ID)
	} else {
		service.AfterAccountCommit(ctx, func(committed context.Context) { r.syncSchedulerAccountSnapshot(committed, account.ID) })
	}
	return nil
}

var _ service.AccountRequestSpeedRepository = (*accountRepository)(nil)
