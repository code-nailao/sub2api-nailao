package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type requestSpeedAccountGroupsStub struct {
	GroupRepository
	groups map[int64]*Group
}

func (s requestSpeedAccountGroupsStub) GetByID(_ context.Context, id int64) (*Group, error) {
	g := s.groups[id]
	if g == nil {
		return nil, ErrGroupNotFound
	}
	return g, nil
}

func TestAccountRequestSpeedMappingsGateByExplicitGroup(t *testing.T) {
	groups := requestSpeedAccountGroupsStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, RequestSpeedSelectionEnabled: true},
		2: {ID: 2, Platform: PlatformOpenAI},
		3: {ID: 3, Platform: PlatformAnthropic, RequestSpeedSelectionEnabled: true},
	}}
	ctx := context.Background()
	clean, err := NormalizeAccountRequestSpeedMappings(ctx, groups, PlatformOpenAI, AccountTypeAPIKey, []int64{1, 2}, map[string][]string{"1": {"ultrafast"}})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"1": {"ultrafast"}}, clean)
	for _, tc := range []struct {
		name           string
		platform, kind string
		mapping        map[string][]string
		expected       error
	}{
		{"ordinary group", PlatformOpenAI, AccountTypeAPIKey, map[string][]string{"2": {"fast"}}, ErrAccountRequestSpeedDisabled},
		{"unassociated group", PlatformOpenAI, AccountTypeAPIKey, map[string][]string{"3": {"fast"}}, ErrAccountRequestSpeedGroup},
		{"oauth account", PlatformOpenAI, AccountTypeOAuth, map[string][]string{"1": {"fast"}}, ErrAccountRequestSpeedInvalid},
		{"different platform", PlatformAnthropic, AccountTypeAPIKey, map[string][]string{"1": {"fast"}}, ErrAccountRequestSpeedInvalid},
		{"invalid tier", PlatformOpenAI, AccountTypeAPIKey, map[string][]string{"1": {"standard"}}, ErrAccountRequestSpeedInvalid},
		{"noncanonical id", PlatformOpenAI, AccountTypeAPIKey, map[string][]string{"01": {"fast"}}, ErrAccountRequestSpeedGroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeAccountRequestSpeedMappings(ctx, groups, tc.platform, tc.kind, []int64{1, 2}, tc.mapping)
			require.ErrorIs(t, err, tc.expected)
		})
	}
	clean, err = NormalizeAccountRequestSpeedMappings(ctx, groups, PlatformOpenAI, AccountTypeAPIKey, []int64{1, 2}, map[string][]string{"1": {"fast"}, "2": {}})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"1": {"fast"}}, clean, "empty disabled assignment can be cleaned without affecting enabled group")
}

func TestAccountRequestSpeedMappingsAreIndependentCopies(t *testing.T) {
	a := &Account{AccountGroups: []AccountGroup{{GroupID: 1, RequestSpeedTiers: []string{"fast"}}, {GroupID: 2}}}
	a.RefreshRequestSpeedMappings()
	require.Equal(t, map[string][]string{"1": {"fast"}}, a.RequestSpeedTiersByGroup)
	a.RequestSpeedTiersByGroup["1"][0] = "ultrafast"
	require.Equal(t, "fast", a.AccountGroups[0].RequestSpeedTiers[0])
	groups, _ := duplicateAccountGroups(a)
	groups[0].RequestSpeedTiers[0] = "ultrafast"
	require.Equal(t, "fast", a.AccountGroups[0].RequestSpeedTiers[0])
}

func TestAccountRequestSpeedMappingsRejectMultipleFixedTiers(t *testing.T) {
	groups := requestSpeedAccountGroupsStub{groups: map[int64]*Group{1: {ID: 1, Platform: PlatformOpenAI, RequestSpeedSelectionEnabled: true}}}
	for _, tiers := range [][]string{{"fast", "ultrafast"}, {"fast", "fast"}} {
		_, err := NormalizeAccountRequestSpeedMappings(context.Background(), groups, PlatformOpenAI, AccountTypeAPIKey, []int64{1}, map[string][]string{"1": tiers})
		require.ErrorIs(t, err, ErrAccountRequestSpeedInvalid, "one supplier Key can only represent one fixed tier")
	}
}

func TestAccountRequestSpeedMappingsMatchAcrossEnabledGroups(t *testing.T) {
	groups := requestSpeedAccountGroupsStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, RequestSpeedSelectionEnabled: true},
		2: {ID: 2, Platform: PlatformOpenAI, RequestSpeedSelectionEnabled: true},
		3: {ID: 3, Platform: PlatformOpenAI},
	}}
	ctx := context.Background()
	_, err := NormalizeAccountRequestSpeedMappings(ctx, groups, PlatformOpenAI, AccountTypeAPIKey, []int64{1, 2, 3}, map[string][]string{"1": {"fast"}, "2": {"ultrafast"}})
	require.ErrorIs(t, err, ErrAccountRequestSpeedInvalid)
	clean, err := NormalizeAccountRequestSpeedMappings(ctx, groups, PlatformOpenAI, AccountTypeAPIKey, []int64{1, 2, 3}, map[string][]string{"1": {"fast"}, "2": {"fast"}, "3": {}})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"1": {"fast"}, "2": {"fast"}}, clean)
}
