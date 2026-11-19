package service

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validRejectionReserveForTest() UpstreamRejectionReserveCommand {
	return UpstreamRejectionReserveCommand{
		RequestID: NewUpstreamRejectionRequestID(), UserID: 1, APIKeyID: 2,
		AccountID: 3, GroupID: 4, Model: "grok-4.7",
		RequestPayloadHash: strings.Repeat("a", 64), UpstreamIdentityHash: strings.Repeat("d", 64), PolicyVersion: "group-policy-v1",
		RuleVersion: "grok_content_rejection_v1", ProviderProfile: "grok_content_rejection_v1",
		Price: 0.1, UpstreamCost: 0.1, UpstreamCurrency: "USD",
	}
}

func TestUpstreamRejectionServerIDAndPriceValidation(t *testing.T) {
	c := validRejectionReserveForTest()
	require.NoError(t, c.Validate())
	require.True(t, IsUpstreamRejectionRequestID(c.RequestID))
	for _, id := range []string{"", "client:one", UpstreamRejectionRequestIDPrefix + "00000000-0000-0000-0000-000000000000", strings.ToUpper(c.RequestID), c.RequestID + ":another"} {
		require.False(t, IsUpstreamRejectionRequestID(id), id)
	}
	for _, amount := range []float64{0, -0.1, math.NaN(), math.Inf(1), math.Inf(-1), 0.100000001, 1e12} {
		bad := c
		bad.Price = amount
		require.ErrorIs(t, bad.Validate(), ErrUpstreamRejectionInvalid)
	}
	for _, amount := range []float64{0.00000001, 0.1, 1.23456789} {
		valid := c
		valid.Price = amount
		require.NoError(t, valid.Validate())
	}
}

func TestUpstreamRejectionSupplierCostMayBeFreeButCustomerPriceMustBePositive(t *testing.T) {
	c := validRejectionReserveForTest()
	c.UpstreamCost = 0
	require.NoError(t, c.Validate())
	for _, cost := range []float64{-0.1, math.NaN(), math.Inf(1), math.Inf(-1), 0.100000001, 1e12} {
		bad := c
		bad.UpstreamCost = cost
		require.ErrorIs(t, bad.Validate(), ErrUpstreamRejectionInvalid)
	}
	c.Price = 0
	require.ErrorIs(t, c.Validate(), ErrUpstreamRejectionInvalid)
}

func TestUpstreamRejectionLedgerAndGroupAmountBoundsAgree(t *testing.T) {
	for _, amount := range []float64{0, -0.1, 0.000000001, 0.00000001, 0.1, 1.23456789, 2000000, 999999999999, 1e12, math.NaN(), math.Inf(1)} {
		c := validRejectionReserveForTest()
		c.Price = amount
		groupErr := ValidateUpstreamRejectionBillingConfig(true, amount)
		require.Equal(t, groupErr == nil, c.Validate() == nil, "saved Group price and runtime admission must agree: %v", amount)
		c = validRejectionReserveForTest()
		c.UpstreamCost = amount
		costErr := ValidateUpstreamRejectionBillingConfig(false, amount)
		require.Equal(t, costErr == nil, c.Validate() == nil, "supplier cost uses the same bound, allowing zero: %v", amount)
	}
}

func TestUpstreamRejectionFingerprintIncludesPriceAndOwnershipSnapshot(t *testing.T) {
	c := validRejectionReserveForTest()
	fingerprint := c.Fingerprint()
	mutations := []func(*UpstreamRejectionReserveCommand){
		func(x *UpstreamRejectionReserveCommand) { x.UserID++ },
		func(x *UpstreamRejectionReserveCommand) { x.APIKeyID++ },
		func(x *UpstreamRejectionReserveCommand) { x.AccountID++ },
		func(x *UpstreamRejectionReserveCommand) { x.GroupID++ },
		func(x *UpstreamRejectionReserveCommand) { x.Price = 0.2 },
		func(x *UpstreamRejectionReserveCommand) { x.UpstreamCost = 0.2 },
		func(x *UpstreamRejectionReserveCommand) { x.PolicyVersion = "v2" },
		func(x *UpstreamRejectionReserveCommand) { x.RuleVersion = "v2" },
		func(x *UpstreamRejectionReserveCommand) { x.RequestPayloadHash = strings.Repeat("b", 64) },
		func(x *UpstreamRejectionReserveCommand) { x.UpstreamIdentityHash = strings.Repeat("e", 64) },
	}
	for _, mutate := range mutations {
		changed := c
		mutate(&changed)
		require.NotEqual(t, fingerprint, changed.Fingerprint())
	}
	copy := c
	require.Equal(t, fingerprint, copy.Fingerprint())
}

func TestUpstreamRejectionIdentityHashPinsEffectiveProviderIdentityOnly(t *testing.T) {
	account := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "fixture-provider-key", "base_url": "https://SUPPLIER.example/v1/",
	}}
	original, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.Len(t, original, 64)
	require.NotContains(t, original, "fixture")
	account.Credentials["base_url"] = "HTTPS://supplier.example/v1"
	canonical, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.Equal(t, original, canonical)
	account.Credentials["model_mapping"] = map[string]string{"grok-4.7": "grok-4.7"}
	rate := 3.0
	account.RateMultiplier = &rate
	cosmetic, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.Equal(t, original, cosmetic)
	account.Credentials["api_key"] = "rotated-fixture-key"
	rotated, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.NotEqual(t, original, rotated)
	account.Credentials["api_key"] = "fixture-provider-key"
	account.Credentials["base_url"] = "https://another.example/v1"
	endpoint, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.NotEqual(t, original, endpoint)
	account.Credentials["base_url"] = "https://supplier.example/v1"
	account.Credentials[credKeyHeaderOverrideEnabled] = true
	account.Credentials[credKeyHeaderOverrides] = map[string]string{"x-relay-token": "fixture-tenant"}
	header, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.NotEqual(t, original, header)
	account.Credentials[credKeyHeaderOverrideEnabled] = false
	disabled, err := UpstreamRejectionIdentityHash(account)
	require.NoError(t, err)
	require.Equal(t, original, disabled)
}

func TestUpstreamRejectionIdentityHashRejectsUnusableIdentity(t *testing.T) {
	for _, credentials := range []map[string]any{
		{"api_key": "", "base_url": "https://supplier.example"},
		{"api_key": "fixture", "base_url": "https://user:pass@supplier.example"},
		{"api_key": "fixture", "base_url": "https://supplier.example?secret=fixture"},
		{"api_key": "fixture", "base_url": "https://supplier.example#fragment"},
		{"api_key": "fixture", "base_url": "file:///tmp/fixture"},
	} {
		_, err := UpstreamRejectionIdentityHash(&Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: credentials})
		require.ErrorIs(t, err, ErrUpstreamRejectionInvalid)
	}
	_, err := UpstreamRejectionIdentityHash(nil)
	require.ErrorIs(t, err, ErrUpstreamRejectionInvalid)
}

func TestUpstreamRejectionFinalizationRequiresExplicitEvidence(t *testing.T) {
	cmd := UpstreamRejectionFinalizeCommand{
		RequestID: NewUpstreamRejectionRequestID(), APIKeyID: 2,
		Outcome:  UpstreamRejectionRejected,
		Evidence: UpstreamRejectionBillingEvidence{RuleID: "refusal", RuleVersion: "grok_content_rejection_v1", OriginalStatus: 403},
	}
	require.NoError(t, cmd.Validate())
	for _, status := range []int{0, 100, 300, 401, 404, 429, 500} {
		bad := cmd
		bad.Evidence.OriginalStatus = status
		require.ErrorIs(t, bad.Validate(), ErrUpstreamRejectionInvalid)
	}
	for _, outcome := range []string{UpstreamRejectionReserved, UpstreamRejectionSent, UpstreamRejectionNormal, "refund"} {
		bad := cmd
		bad.Outcome = outcome
		require.ErrorIs(t, bad.Validate(), ErrUpstreamRejectionInvalid)
	}
	for _, mutation := range []func(*UpstreamRejectionFinalizeCommand){
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.RuleID = "" },
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.InputTokens = -1 },
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.ResponseDigest = "raw content is not a digest" },
	} {
		bad := cmd
		mutation(&bad)
		require.ErrorIs(t, bad.Validate(), ErrUpstreamRejectionInvalid)
	}
}
