//go:build unit

package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamRejectionPriceValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		price   float64
		valid   bool
	}{
		{"disabled zero", false, 0, true},
		{"default", true, 0.1, true},
		{"eight decimals", true, 0.12345678, true},
		{"smallest charge", true, 0.00000001, true},
		{"zero enabled", true, 0, false},
		{"negative disabled", false, -0.1, false},
		{"NaN", false, math.NaN(), false},
		{"positive infinity", true, math.Inf(1), false},
		{"negative infinity", false, math.Inf(-1), false},
		{"nine decimals", false, 0.123456789, false},
		{"under precision", true, 0.000000001, false},
		{"numeric overflow", false, 1e12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUpstreamRejectionBillingConfig(tc.enabled, tc.price)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

type rejectionGroupAuthInvalidator struct {
	APIKeyAuthCacheInvalidator
	groups []int64
}

func (s *rejectionGroupAuthInvalidator) InvalidateAuthCacheByGroupID(_ context.Context, id int64) {
	s.groups = append(s.groups, id)
}

func TestAdminGroupUpstreamRejectionConfigDefaultsAndUpdates(t *testing.T) {
	ctx := context.Background()
	repo := &groupRepoStubForAdmin{createID: 71}
	invalidator := &rejectionGroupAuthInvalidator{}
	svc := &adminServiceImpl{groupRepo: repo, authCacheInvalidator: invalidator}
	group, err := svc.CreateGroup(ctx, &CreateGroupInput{Name: "rejection-config", Platform: PlatformGrok, RateMultiplier: 0.2})
	require.NoError(t, err)
	require.False(t, group.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.1, group.UpstreamRejectionPrice)
	repo.getByID = group

	enabled, custom := true, 0.12345678
	updated, err := svc.UpdateGroup(ctx, group.ID, &UpdateGroupInput{UpstreamRejectionBillingEnabled: &enabled, UpstreamRejectionPrice: &custom})
	require.NoError(t, err)
	require.True(t, updated.UpstreamRejectionBillingEnabled)
	require.Equal(t, custom, updated.UpstreamRejectionPrice)
	require.Equal(t, 0.2, updated.RateMultiplier)
	require.Equal(t, []int64{group.ID}, invalidator.groups)

	description := "cosmetic"
	updated, err = svc.UpdateGroup(ctx, group.ID, &UpdateGroupInput{Description: &description})
	require.NoError(t, err)
	require.True(t, updated.UpstreamRejectionBillingEnabled)
	require.Equal(t, custom, updated.UpstreamRejectionPrice)

	disabled := false
	updated, err = svc.UpdateGroup(ctx, group.ID, &UpdateGroupInput{UpstreamRejectionBillingEnabled: &disabled})
	require.NoError(t, err)
	require.False(t, updated.UpstreamRejectionBillingEnabled)
	require.Equal(t, custom, updated.UpstreamRejectionPrice, "turning off must retain the configured price")
}

func TestAdminGroupUpstreamRejectionInvalidCombinedConfigDoesNotPersist(t *testing.T) {
	ctx := context.Background()
	enabled, zero, tooPrecise := true, 0.0, 0.123456789
	for _, input := range []*CreateGroupInput{
		{Name: "invalid-zero", RateMultiplier: 1, UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: &zero},
		{Name: "invalid-precision", RateMultiplier: 1, UpstreamRejectionPrice: &tooPrecise},
	} {
		repo := &groupRepoStubForAdmin{}
		_, err := (&adminServiceImpl{groupRepo: repo}).CreateGroup(ctx, input)
		require.Error(t, err)
		require.Nil(t, repo.created)
	}
	repo := &groupRepoStubForAdmin{getByID: &Group{ID: 72, Platform: PlatformGrok, UpstreamRejectionPrice: 0}}
	invalidator := &rejectionGroupAuthInvalidator{}
	_, err := (&adminServiceImpl{groupRepo: repo, authCacheInvalidator: invalidator}).UpdateGroup(ctx, 72,
		&UpdateGroupInput{UpstreamRejectionBillingEnabled: &enabled})
	require.Error(t, err, "enabling must validate the stored price even when the price field is omitted")
	require.Nil(t, repo.updated)
	require.Empty(t, invalidator.groups)
}

func TestAdminGroupUpstreamRejectionBillingScopeIsExplicit(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		platform     string
		subscription string
	}{
		{PlatformOpenAI, SubscriptionTypeStandard},
		{PlatformComposite, SubscriptionTypeStandard},
		{PlatformGrok, SubscriptionTypeSubscription},
	} {
		t.Run(tc.platform+"/"+tc.subscription, func(t *testing.T) {
			repo := &groupRepoStubForAdmin{}
			_, err := (&adminServiceImpl{groupRepo: repo}).CreateGroup(ctx, &CreateGroupInput{Name: "invalid-scope",
				Platform: tc.platform, SubscriptionType: tc.subscription, RateMultiplier: 1, UpstreamRejectionBillingEnabled: true})
			require.ErrorContains(t, err, "Grok standard group")
			require.Nil(t, repo.created)
		})
	}
	for _, disable := range []bool{false, true} {
		for _, input := range []*UpdateGroupInput{{Platform: PlatformOpenAI}, {SubscriptionType: SubscriptionTypeSubscription}} {
			repo := &groupRepoStubForAdmin{getByID: &Group{ID: 76, Platform: PlatformGrok, SubscriptionType: SubscriptionTypeStandard,
				UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: 0.1}}
			if disable {
				off := false
				input.UpstreamRejectionBillingEnabled = &off
			}
			updated, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(ctx, 76, input)
			if disable {
				require.NoError(t, err)
				require.False(t, updated.UpstreamRejectionBillingEnabled)
				require.Equal(t, 0.1, updated.UpstreamRejectionPrice)
			} else {
				require.ErrorContains(t, err, "explicitly disable")
				require.Nil(t, repo.updated)
			}
		}
	}
}

func TestAPIKeyAuthSnapshotUpstreamRejectionPolicyRoundtrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		groupID := int64(73)
		key := &APIKey{ID: 74, UserID: 75, GroupID: &groupID, Key: "rejection-cache-fixture", Status: StatusActive,
			User: &User{ID: 75, Status: StatusActive}, Group: &Group{ID: groupID, Platform: PlatformGrok,
				Status: StatusActive, UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.12345678, RateMultiplier: 0.2}}
		svc := &APIKeyService{}
		data, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
		require.NoError(t, err)
		var cached APIKeyAuthCacheEntry
		require.NoError(t, json.Unmarshal(data, &cached))
		materialized, used, err := svc.applyAuthCacheEntry(key.Key, &cached)
		require.NoError(t, err)
		require.True(t, used)
		require.Equal(t, enabled, materialized.Group.UpstreamRejectionBillingEnabled)
		require.Equal(t, 0.12345678, materialized.Group.UpstreamRejectionPrice)
		require.Equal(t, 0.2, materialized.Group.RateMultiplier)

		cached.Snapshot.Version = 25
		materialized, used, err = svc.applyAuthCacheEntry(key.Key, &cached)
		require.NoError(t, err)
		require.False(t, used, "an older serving instance must not repopulate Redis with a silently disabled policy")
		require.Nil(t, materialized)
	}
}

func TestUpstreamRejectionPriceDisclosureViewsUseGroupPrice(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		groups := []Group{{ID: 10, Name: "disclosure", Platform: PlatformGrok, RateMultiplier: 0.2,
			UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.1}}
		channels := []Channel{plazaPricedChannel(1, "grok-channel", []int64{10}, PlatformGrok, "grok-fixture")}
		available, err := newAvailableChannelService(channels, &stubGroupRepoForAvailable{activeGroups: groups}).ListAvailable(context.Background())
		require.NoError(t, err)
		require.Len(t, available, 1)
		require.Len(t, available[0].Groups, 1)
		require.Equal(t, enabled, available[0].Groups[0].UpstreamRejectionBillingEnabled)
		require.Equal(t, 0.1, available[0].Groups[0].UpstreamRejectionPrice)
		plaza, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())
		require.NoError(t, err)
		require.Len(t, plaza, 1)
		require.Equal(t, enabled, plaza[0].UpstreamRejectionBillingEnabled)
		require.Equal(t, 0.1, plaza[0].UpstreamRejectionPrice)
	}
}

func TestDuplicateGroupPreservesUpstreamRejectionPolicyAndRemainsInactive(t *testing.T) {
	source := &Group{ID: 78, Name: "grok-rejection-source", Platform: PlatformGrok, SubscriptionType: SubscriptionTypeStandard,
		Status: StatusActive, RateMultiplier: 0.2, UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: 0.12345678}
	repo := newDuplicateGroupRepoStub(source)
	svc := &adminServiceImpl{groupRepo: repo, groupDuplicateRepo: repo}
	duplicate, err := svc.DuplicateGroup(context.Background(), source.ID, "admin:fixture", "rejection-policy")
	require.NoError(t, err)
	require.True(t, duplicate.UpstreamRejectionBillingEnabled)
	require.Equal(t, 0.12345678, duplicate.UpstreamRejectionPrice)
	require.Equal(t, duplicateGroupInactiveStatus, duplicate.Status)
}
