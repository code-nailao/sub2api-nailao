package payment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// BEPusdtNetwork is the reviewed protocol catalog, not a list of enabled channels.
// Enabled networks and optional receiving addresses belong to each provider instance.
// TON/memo-based deposits require a separate memo-aware adapter and are not advertised.
type BEPusdtNetwork struct{ Method, TradeType, Network, Label, AddressKind string }

var bepNetworks = []BEPusdtNetwork{
	{TypeUSDTBSC, "usdt.bep20", "bsc", "BSC (BEP20)", "evm"},
	{TypeUSDTArbitrum, "usdt.arbitrum", "arbitrum", "Arbitrum One", "evm"},
	{"usdt_tron", "usdt.trc20", "tron", "TRON (TRC20)", "tron"},
	{"usdt_ethereum", "usdt.erc20", "ethereum", "Ethereum (ERC20)", "evm"},
	{"usdt_polygon", "usdt.polygon", "polygon", "Polygon", "evm"},
	{"usdt_solana", "usdt.solana", "solana", "Solana", "solana"},
	{"usdt_aptos", "usdt.aptos", "aptos", "Aptos", "aptos"},
	{"usdt_xlayer", "usdt.xlayer", "xlayer", "X Layer", "evm"},
	{"usdt_plasma", "usdt.plasma", "plasma", "Plasma", "evm"},
}

func BEPusdtNetworks() []BEPusdtNetwork { return append([]BEPusdtNetwork(nil), bepNetworks...) }
func BEPusdtNetworkByMethod(method string) (BEPusdtNetwork, bool) {
	for _, n := range bepNetworks {
		if n.Method == method {
			return n, true
		}
	}
	return BEPusdtNetwork{}, false
}
func BEPusdtNetworkByID(network string) (BEPusdtNetwork, bool) {
	for _, n := range bepNetworks {
		if n.Network == network {
			return n, true
		}
	}
	return BEPusdtNetwork{}, false
}
func BEPusdtNetworkByTradeType(tradeType string) (BEPusdtNetwork, bool) {
	for _, n := range bepNetworks {
		if n.TradeType == tradeType {
			return n, true
		}
	}
	return BEPusdtNetwork{}, false
}

var bepHexAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
var bepHexHash = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
var bepTronHash = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

const bepBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func bepDecodeBase58(s string) ([]byte, bool) {
	if len(s) == 0 || len(s) > 128 {
		return nil, false
	}
	n := new(big.Int)
	base := big.NewInt(58)
	for _, c := range s {
		i := strings.IndexRune(bepBase58Alphabet, c)
		if i < 0 {
			return nil, false
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(i)))
	}
	prefix := len(s) - len(strings.TrimLeft(s, "1"))
	return append(make([]byte, prefix), n.Bytes()...), true
}
func (n BEPusdtNetwork) CanonicalAddress(s string) (string, bool) {
	if s == "" || s != strings.TrimSpace(s) {
		return "", false
	}
	switch n.AddressKind {
	case "evm":
		return strings.ToLower(s), bepHexAddress.MatchString(s)
	case "aptos":
		return strings.ToLower(s), bepHexHash.MatchString(s)
	case "tron":
		b, ok := bepDecodeBase58(s)
		if !ok || len(b) != 25 || b[0] != 0x41 {
			return "", false
		}
		first := sha256.Sum256(b[:21])
		second := sha256.Sum256(first[:])
		return s, bytes.Equal(b[21:], second[:4])
	case "solana":
		b, ok := bepDecodeBase58(s)
		return s, ok && len(b) == 32
	default:
		return "", false
	}
}
func (n BEPusdtNetwork) CanonicalHash(s string) (string, bool) {
	switch n.AddressKind {
	case "evm", "aptos":
		return strings.ToLower(s), bepHexHash.MatchString(s)
	case "tron":
		return strings.ToLower(s), bepTronHash.MatchString(s)
	case "solana":
		b, ok := bepDecodeBase58(s)
		return s, ok && len(b) == 64
	default:
		return "", false
	}
}
func (n BEPusdtNetwork) SameAddress(a, b string) bool {
	ca, ok := n.CanonicalAddress(a)
	cb, ok2 := n.CanonicalAddress(b)
	return ok && ok2 && ca == cb
}

// Addresses are public receive-only data. The adapter never requests exchange keys.
// Empty entries preserve Pay's existing wallet-pool allocation. Explicit addresses
// use the native signed `address` field; this does not mutate Pay's shared wallet pool.
func ParseBEPusdtWalletAddresses(raw string) (map[string]string, error) {
	addresses := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return addresses, nil
	}
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &addresses) != nil || addresses == nil {
		return nil, fmt.Errorf("walletAddresses must be an object of payment method to address")
	}
	for method, value := range addresses {
		n, ok := BEPusdtNetworkByMethod(method)
		if !ok {
			return nil, fmt.Errorf("unknown USDT network: %s", method)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			delete(addresses, method)
			continue
		}
		canonical, ok := n.CanonicalAddress(value)
		if !ok {
			return nil, fmt.Errorf("invalid receiving address for %s", method)
		}
		addresses[method] = canonical
	}
	return addresses, nil
}
