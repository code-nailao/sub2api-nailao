package payment

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestBEPusdtNetworkCatalogAndAddressValidation(t *testing.T) {
	require.Len(t, BEPusdtNetworks(), 9)
	for _, n := range BEPusdtNetworks() {
		t.Run(n.Method, func(t *testing.T) {
			trade, network, ok := BEPusdtTradeType(n.Method)
			require.True(t, ok)
			require.Equal(t, n.TradeType, trade)
			require.Equal(t, n.Network, network)
			address := "0x" + strings.Repeat("a", 40)
			hash := "0x" + strings.Repeat("B", 64)
			switch n.AddressKind {
			case "tron":
				address = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
				hash = strings.Repeat("B", 64)
			case "solana":
				address = strings.Repeat("1", 32)
				hash = strings.Repeat("1", 64)
			case "aptos":
				address = "0x" + strings.Repeat("a", 64)
			}
			canonical, ok := n.CanonicalAddress(address)
			require.True(t, ok)
			require.True(t, n.SameAddress(address, canonical))
			_, ok = n.CanonicalHash(hash)
			require.True(t, ok)
			_, ok = n.CanonicalAddress(address + " ")
			require.False(t, ok)
			_, ok = n.CanonicalHash("not-a-hash")
			require.False(t, ok)
		})
	}
	tron, _ := BEPusdtNetworkByMethod("usdt_tron")
	_, ok := tron.CanonicalAddress("T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwa")
	require.False(t, ok)
	_, ok = tron.CanonicalAddress("t9yd14nj9j7xab4dbgeix9h8unkkhxuw wb")
	require.False(t, ok)
	sol, _ := BEPusdtNetworkByMethod("usdt_solana")
	a := "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	require.False(t, sol.SameAddress(a, strings.ToLower(a)))
}
func TestBEPusdtWalletOverridesRejectUnknownNetworksAndBadAddresses(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"usdt_base":"0x1111111111111111111111111111111111111111"}`, `{"usdt_tron":"0x1111111111111111111111111111111111111111"}`, `{"usdt_bsc":123}`} {
		_, e := ParseBEPusdtWalletAddresses(raw)
		require.Error(t, e)
	}
	m, e := ParseBEPusdtWalletAddresses(`{"usdt_bsc":"0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","usdt_arbitrum":""}`)
	require.NoError(t, e)
	require.Equal(t, "0x"+strings.Repeat("a", 40), m[TypeUSDTBSC])
	require.NotContains(t, m, TypeUSDTArbitrum)
}

func TestBEPusdtDiscardsRetiredPresentationConfig(t *testing.T) {
	cfg := map[string]string{"checkoutPresentation": "main", "checkoutCopy": "obsolete", "apiToken": "test-only", "rateSource": "gateway"}
	got := NativeBEPConfig(cfg)
	require.NotContains(t, got, "checkoutPresentation")
	require.NotContains(t, got, "checkoutCopy")
	require.Equal(t, "test-only", got["apiToken"])
	require.Contains(t, cfg, "checkoutPresentation")
}
