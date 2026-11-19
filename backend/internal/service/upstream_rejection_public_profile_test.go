//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeLegacyRejectionProfile stands in for a deployment's privately configured
// legacy id. It is deliberately not any real identifier.
const fakeLegacyRejectionProfile = "legacy_profile_v0"

func setLegacyRejectionProfileForTest(t *testing.T, id string) {
	t.Helper()
	SetUpstreamRejectionLegacyProfile(id)
	t.Cleanup(func() { SetUpstreamRejectionLegacyProfile("") })
}

func rejectionProfileAccount(profile string) *Account {
	return &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Extra: map[string]any{
		"upstream_rejection_profile": profile, "upstream_rejection_cost": .12, "upstream_rejection_currency": "usd",
	}}
}

func rejectionProfileReserveCommand(profile string) *UpstreamRejectionReserveCommand {
	cmd := validRejectionReserveForTest()
	cmd.ProviderProfile = profile
	return &cmd
}

func TestUpstreamRejectionProfileWithoutLegacyConfigUsesCanonicalID(t *testing.T) {
	setLegacyRejectionProfileForTest(t, "")
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, UpstreamRejectionProfileID())
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, UpstreamRejectionRuleVersion())

	account := rejectionProfileAccount(UpstreamRejectionProfileGrokContentV1)
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, account.GetUpstreamRejectionProfile())
	evidence := DetectUpstreamRejectionHTTP(account, 403, []byte(`{"error":{"message":"content moderation"}}`))
	require.NotNil(t, evidence)
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, evidence.Profile)
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, evidence.RuleVersion)
	require.NoError(t, rejectionProfileReserveCommand(UpstreamRejectionProfileGrokContentV1).Validate())

	// Without configuration no other id is known, including one that a
	// deployment would configure: it must not enable billing on its own.
	for _, unknown := range []string{fakeLegacyRejectionProfile, "auto", "GROK_CONTENT_REJECTION_V1", ""} {
		require.False(t, IsUpstreamRejectionProfile(unknown), unknown)
		require.False(t, IsUpstreamRejectionLegacyProfile(unknown), unknown)
		require.Empty(t, rejectionProfileAccount(unknown).GetUpstreamRejectionProfile(), unknown)
		require.ErrorIs(t, rejectionProfileReserveCommand(unknown).Validate(), ErrUpstreamRejectionInvalid, unknown)
	}
}

func TestUpstreamRejectionProfileWithLegacyConfigKeepsStoredContract(t *testing.T) {
	setLegacyRejectionProfileForTest(t, fakeLegacyRejectionProfile)
	require.Equal(t, fakeLegacyRejectionProfile, UpstreamRejectionProfileID())
	require.Equal(t, fakeLegacyRejectionProfile, UpstreamRejectionRuleVersion())
	require.True(t, IsUpstreamRejectionLegacyProfile(fakeLegacyRejectionProfile))
	require.False(t, IsUpstreamRejectionLegacyProfile(UpstreamRejectionProfileGrokContentV1))

	var original *UpstreamRejectionEvidence
	for _, profile := range []string{fakeLegacyRejectionProfile, UpstreamRejectionProfileGrokContentV1} {
		account := rejectionProfileAccount(profile)
		// Both spellings configured on an account resolve to the id already in
		// stored rows, so new rows stay identical to the existing ones.
		require.Equal(t, fakeLegacyRejectionProfile, account.GetUpstreamRejectionProfile())
		require.Equal(t, .12, account.GetUpstreamRejectionCost())
		require.Equal(t, "USD", account.GetUpstreamRejectionCurrency())
		evidence := DetectUpstreamRejectionHTTP(account, 403, []byte(`{"error":{"message":"content moderation"}}`))
		require.NotNil(t, evidence)
		require.Equal(t, fakeLegacyRejectionProfile, evidence.Profile)
		require.Equal(t, fakeLegacyRejectionProfile, evidence.RuleVersion)
		if original == nil {
			original = evidence
		} else {
			require.Equal(t, original, evidence)
		}
		require.Equal(t, profile, account.Extra["upstream_rejection_profile"], "normalization is read-only")
		require.ErrorContains(t, validateGrokRejectionTaskResponse(account, "owned-task", []byte(`{"request_id":"foreign-task"}`)), "identity mismatch")
		require.NoError(t, validateGrokRejectionTaskResponse(account, "owned-task", []byte(`{"request_id":"owned-task"}`)))
		// Stored reservations of either spelling remain reconcilable.
		require.True(t, IsUpstreamRejectionProfile(profile))
		require.NoError(t, rejectionProfileReserveCommand(profile).Validate())
	}
	require.Empty(t, rejectionProfileAccount("auto").GetUpstreamRejectionProfile())
	require.ErrorIs(t, rejectionProfileReserveCommand("legacy_profile_v1").Validate(), ErrUpstreamRejectionInvalid)

	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformGrok, Type: AccountTypeOAuth},
	} {
		for _, profile := range []string{fakeLegacyRejectionProfile, UpstreamRejectionProfileGrokContentV1} {
			account.Extra = map[string]any{"upstream_rejection_profile": profile}
			require.Empty(t, account.GetUpstreamRejectionProfile(), "an alias must not broaden eligible account types")
		}
	}
}

func TestUpstreamRejectionLegacyProfilePublishedFromGatewayConfig(t *testing.T) {
	t.Cleanup(func() { SetUpstreamRejectionLegacyProfile("") })
	cfg := &config.Config{}
	cfg.Gateway.UpstreamRejectionLegacyProfile = fakeLegacyRejectionProfile
	_ = NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, NewBillingService(cfg, nil), nil, &BillingCacheService{}, nil, &DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	require.Equal(t, fakeLegacyRejectionProfile, UpstreamRejectionProfileID())
	cfg.Gateway.UpstreamRejectionLegacyProfile = ""
	_ = NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, NewBillingService(cfg, nil), nil, &BillingCacheService{}, nil, &DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	require.Equal(t, UpstreamRejectionProfileGrokContentV1, UpstreamRejectionProfileID())
}
