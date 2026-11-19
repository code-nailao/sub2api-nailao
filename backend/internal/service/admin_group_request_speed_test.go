//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeAdminGroupRequestSpeedPreservesFixedPolicy(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 8}
	svc := &adminServiceImpl{groupRepo: repo}
	g, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name: "fixed suppliers", Platform: PlatformOpenAI, RateMultiplier: 1,
		RequestSpeedSelectionEnabled: true, DefaultRequestSpeedTier: RequestSpeedFast,
		ForceOpenAIUltrafast: true, OpenAIServiceTierPolicy: GroupTierForceUltrafast,
	})
	require.NoError(t, err)
	require.True(t, g.RequestSpeedSelectionEnabled)
	require.Equal(t, RequestSpeedFast, g.DefaultRequestSpeedTier)
	require.True(t, g.ForceOpenAIUltrafast, "selection suspends rather than destroys prior policy")
	require.Equal(t, GroupTierForceUltrafast, g.OpenAIServiceTierPolicy)

	repo.getByID = g
	invalidator := &authCacheInvalidatorStub{}
	svc.authCacheInvalidator = invalidator
	newDefault := RequestSpeedUltrafast
	_, err = svc.UpdateGroup(context.Background(), g.ID, &UpdateGroupInput{DefaultRequestSpeedTier: &newDefault})
	require.NoError(t, err)
	require.True(t, repo.updated.RequestSpeedSelectionEnabled)
	require.Equal(t, newDefault, repo.updated.DefaultRequestSpeedTier)
	require.Equal(t, []int64{g.ID}, invalidator.groupIDs)
}

func TestNativeAdminGroupRequestSpeedPlatformGuard(t *testing.T) {
	repo := &groupRepoStubForAdmin{getByID: &Group{ID: 7, Name: "selectable", Platform: PlatformOpenAI,
		RequestSpeedSelectionEnabled: true, DefaultRequestSpeedTier: RequestSpeedUltrafast}}
	svc := &adminServiceImpl{groupRepo: repo}
	_, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{Platform: PlatformComposite})
	require.Error(t, err)
	require.Nil(t, repo.updated, "platform switch cannot leave an enabled invalid group")
	_, err = svc.CreateGroup(context.Background(), &CreateGroupInput{Name: "wrong platform", Platform: PlatformAnthropic,
		RateMultiplier: 1, RequestSpeedSelectionEnabled: true})
	require.Error(t, err)
	require.Nil(t, repo.created)
}
