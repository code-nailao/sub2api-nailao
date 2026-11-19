package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// fakeLegacyRejectionProfile stands in for a deployment's privately configured
// legacy profile id; it is deliberately not any real identifier.
const fakeLegacyRejectionProfile = "legacy_profile_v0"

func setLegacyRejectionProfileForTest(t *testing.T, id string) {
	t.Helper()
	service.SetUpstreamRejectionLegacyProfile(id)
	t.Cleanup(func() { service.SetUpstreamRejectionLegacyProfile("") })
}

func TestUpstreamRejectionUsagePrivacyUserWhitelistAdminEvidenceUnchanged(t *testing.T) {
	for _, tc := range []struct{ legacy, ruleVersion string }{
		{"", service.UpstreamRejectionProfileGrokContentV1},
		{fakeLegacyRejectionProfile, fakeLegacyRejectionProfile},
		{fakeLegacyRejectionProfile, service.UpstreamRejectionProfileGrokContentV1},
	} {
		t.Run(tc.ruleVersion, func(t *testing.T) {
			setLegacyRejectionProfileForTest(t, tc.legacy)
			assertUpstreamRejectionUsagePrivacy(t, tc.ruleVersion)
		})
	}
}

func assertUpstreamRejectionUsagePrivacy(t *testing.T, ruleVersion string) {
	snapshot := &service.UsageBillingSnapshot{
		ChargeKind: "upstream_rejection", FixedCharge: .1, OriginalStatus: 403,
		RejectionRuleID: "supplier-private-rule", RejectionRuleVersion: ruleVersion, Version: 7,
		ForwardedTier: "private-forwarded", ObservedTier: "private-observed", BilledTier: "private-billed",
		GroupMultiplier: 9, EffectiveRateMultiplier: 1,
		EffectiveUnitPrices: map[string]float64{"request": .1, "supplier_cost": .037},
	}
	log := &service.UsageLog{RequestID: "customer-reference", Model: "grok-4.6", ActualCost: .1, TotalCost: .1,
		BillingSnapshot: snapshot, Account: &service.Account{ID: 7153, Name: "private-supplier-name"}}
	before, err := json.Marshal(snapshot)
	require.NoError(t, err)
	userJSON, err := json.Marshal(UsageLogFromService(log))
	require.NoError(t, err)
	var user map[string]any
	require.NoError(t, json.Unmarshal(userJSON, &user))
	publicSnapshot, ok := user["billing_snapshot"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"charge_kind": "content_rejection", "fixed_charge": .1}, publicSnapshot)
	require.Equal(t, .1, user["actual_cost"])
	require.Equal(t, "customer-reference", user["request_id"])
	// Neither profile id (canonical or legacy) nor the rule id reaches a user.
	for _, private := range []string{service.UpstreamRejectionProfileGrokContentV1, fakeLegacyRejectionProfile, "supplier-private-rule", "private-supplier-name", "supplier_cost", "original_status", "rejection_rule_id", "rejection_rule_version", "private-forwarded", "private-observed", "private-billed"} {
		require.NotContains(t, string(userJSON), private)
	}
	adminJSON, err := json.Marshal(UsageLogFromServiceAdmin(log))
	require.NoError(t, err)
	var admin struct {
		BillingSnapshot json.RawMessage `json:"billing_snapshot"`
	}
	require.NoError(t, json.Unmarshal(adminJSON, &admin))
	require.JSONEq(t, string(before), string(admin.BillingSnapshot))
	after, err := json.Marshal(log.BillingSnapshot)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "user serialization must never mutate internal audit evidence")
}

func TestUpstreamRejectionUsagePrivacyPreservesOrdinaryAndAbsentSnapshots(t *testing.T) {
	requested := "priority"
	ordinary := &service.UsageBillingSnapshot{Version: 1, RequestedTier: &requested, ForwardedTier: "priority",
		ObservedTier: "priority", BilledTier: "priority", GroupMultiplier: .13, EffectiveRateMultiplier: .26,
		TierMultiplier: 2, ChargedTierMultiplier: 2, EffectiveUnitPrices: map[string]float64{"input": .000002}}
	expected, err := json.Marshal(ordinary)
	require.NoError(t, err)
	for _, snapshot := range []*service.UsageBillingSnapshot{nil, ordinary} {
		log := &service.UsageLog{BillingSnapshot: snapshot}
		for _, value := range []any{UsageLogFromService(log), UsageLogFromServiceAdmin(log)} {
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			if snapshot == nil {
				require.NotContains(t, fields, "billing_snapshot")
			} else {
				require.JSONEq(t, string(expected), string(fields["billing_snapshot"]))
			}
		}
	}
}

func TestUpstreamRejectionAccountPrivacyUsesPublicProfileWithoutMutation(t *testing.T) {
	for _, tc := range []struct{ legacy, stored string }{
		{"", service.UpstreamRejectionProfileGrokContentV1},
		{fakeLegacyRejectionProfile, fakeLegacyRejectionProfile},
		{fakeLegacyRejectionProfile, service.UpstreamRejectionProfileGrokContentV1},
	} {
		setLegacyRejectionProfileForTest(t, tc.legacy)
		stored := tc.stored
		account := &service.Account{ID: 7153, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
			Extra: map[string]any{"upstream_rejection_profile": stored, "upstream_rejection_cost": .12, "ordinary": "unchanged"}}
		full, shallow := AccountFromService(account), AccountFromServiceShallow(account)
		for _, value := range []any{full, shallow, AccountListItemFromAccount(full)} {
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"upstream_rejection_profile":"grok_content_rejection_v1"`)
			require.NotContains(t, string(encoded), fakeLegacyRejectionProfile, "a configured legacy id is shown as the canonical id")
		}
		require.Equal(t, stored, account.Extra["upstream_rejection_profile"])
		require.Equal(t, .12, full.Extra["upstream_rejection_cost"], "admin retains cost controls")
		require.Equal(t, "unchanged", full.Extra["ordinary"])
	}
	setLegacyRejectionProfileForTest(t, "")
	// Invalid stored metadata must not panic while rendering an admin account.
	malformed := &service.Account{Extra: map[string]any{"upstream_rejection_profile": map[string]any{"invalid": true}}}
	require.NotPanics(t, func() { _ = AccountFromService(malformed) })
}
