// Package bridge moves a user's deposits on EVM chains to their Solana wallet.
package bridge

import (
	"fmt"
	"math/big"
	"slices"
)

type Chain string

// Chain values are Privy's chain names; Privy spells BNB Chain "bsc".
const (
	Ethereum  Chain = "ethereum"
	BNB       Chain = "bsc"
	Base      Chain = "base"
	Arbitrum  Chain = "arbitrum"
	Optimism  Chain = "optimism"
	Polygon   Chain = "polygon"
	Monad     Chain = "monad"
	Tempo     Chain = "tempo"
	Robinhood Chain = "robinhood"
	MegaETH   Chain = "megaeth"
)

type Asset string

const (
	USDC  Asset = "usdc"
	USDT  Asset = "usdt"
	PYUSD Asset = "pyusd"
	USDG  Asset = "usdg"
	DAI   Asset = "dai"
	USDe  Asset = "usde"
)

// AssetInfo is how the deposit sheet shows an asset. Icon is the sheet's
// image file, empty while the sheet has none.
type AssetInfo struct {
	Symbol string
	Name   string
	Stable bool
	Icon   string
}

var Assets = map[Asset]AssetInfo{
	USDC:  {"USDC", "USD Coin", true, "usdc.png"},
	USDT:  {"USDT", "Tether USD", true, "usdt.png"},
	PYUSD: {"PYUSD", "PayPal USD", true, "pyusd.svg"},
	USDG:  {"USDG", "Global Dollar", true, "usdg.svg"},
	DAI:   {"DAI", "Dai", true, ""},
	USDe:  {"USDe", "Ethena USDe", true, ""},
}

func init() {
	for _, n := range Networks {
		for _, t := range n.Tokens {
			if _, ok := Assets[t.Asset]; !ok {
				panic(fmt.Sprintf("bridge: %s on %s has no entry in Assets", t.Asset, n.Chain))
			}
		}
	}
}

// NativeToken is the address Privy and Relay use for a chain's own coin.
const NativeToken = "0x0000000000000000000000000000000000000000"

// RouteSet names the cells a Privy deposit account was created to convert.
// Privy cannot add sources to an existing account, so an account keeps the set
// it was made with, and a wider set is a new entry at the end of RouteSets.
type RouteSet string

const (
	// Legacy13 accounts were created with Privy's usdc and usdt aliases on
	// five chains plus eth on three; they still convert eth, which Loyal no
	// longer offers.
	Legacy13  RouteSet = "legacy-13"
	StablesV1 RouteSet = "stables-v1"
)

// RouteSets runs oldest first, and each set holds every cell of the sets
// before it.
var RouteSets = []RouteSet{Legacy13, StablesV1}

// Current is the set new deposit accounts are created with and Relay bridges.
var Current = RouteSets[len(RouteSets)-1]

func ParseRouteSet(s string) (RouteSet, bool) {
	set := RouteSet(s)
	return set, slices.Contains(RouteSets, set)
}

// Networks is the part of the table the set offers, in table order.
func (s RouteSet) Networks() []Network {
	rank := slices.Index(RouteSets, s)
	var out []Network
	for _, n := range Networks {
		var tokens []Token
		for _, t := range n.Tokens {
			if slices.Index(RouteSets, t.Since) <= rank {
				tokens = append(tokens, t)
			}
		}
		if len(tokens) > 0 {
			n.Tokens = tokens
			out = append(out, n)
		}
	}
	return out
}

type Token struct {
	Asset    Asset
	Address  string
	Decimals uint8
	Since    RouteSet
}

type Network struct {
	Chain   Chain
	Name    string
	Icon    string
	ChainID int64
	// MinUSD is the smallest deposit worth bridging; Ethereum gas makes
	// small transfers cost more than they move.
	MinUSD int
	Tokens []Token
}

type BelowMinimum struct {
	Chain  Chain
	MinUSD int
}

func (e *BelowMinimum) Error() string {
	return fmt.Sprintf("bridging from %s starts at %d USD", e.Chain, e.MinUSD)
}

func (n Network) checkMinimum(amountUSD string) error {
	if n.MinUSD == 0 {
		return nil
	}
	usd, ok := new(big.Rat).SetString(amountUSD)
	if !ok {
		return fmt.Errorf("quote: details.currencyIn.amountUsd %q is not a number", amountUSD)
	}
	if usd.Cmp(big.NewRat(int64(n.MinUSD), 1)) < 0 {
		return &BelowMinimum{Chain: n.Chain, MinUSD: n.MinUSD}
	}
	return nil
}

func (n Network) Token(a Asset) (Token, bool) {
	for _, t := range n.Tokens {
		if t.Asset == a {
			return t, true
		}
	}
	return Token{}, false
}

// Networks holds the stablecoins that both Privy auto-bridge and a direct
// Relay deposit turn into USDC on Solana. USDT0 is listed as usdt, and on
// Optimism usdt is the bridged legacy USDT. BNB Chain's USDC and USDT and
// every DAI and USDe use 18 decimals.
var Networks = []Network{
	{Chain: Ethereum, Name: "Ethereum", Icon: "ethereum.png", ChainID: 1, MinUSD: 20, Tokens: []Token{
		{USDC, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", 6, Legacy13},
		{USDT, "0xdac17f958d2ee523a2206206994597c13d831ec7", 6, Legacy13},
		{PYUSD, "0x6c3ea9036406852006290770bedfcaba0e23a0e8", 6, StablesV1},
		{USDG, "0xe343167631d89b6ffc58b88d6b7fb0228795491d", 6, StablesV1},
		{DAI, "0x6b175474e89094c44da98b954eedeac495271d0f", 18, StablesV1},
		{USDe, "0x4c9edd5852cd905f086c759e8383e09bff1e68b3", 18, StablesV1},
	}},
	{Chain: BNB, Name: "BNB Smart Chain", Icon: "bnb.png", ChainID: 56, Tokens: []Token{
		{USDC, "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", 18, Legacy13},
		{USDT, "0x55d398326f99059ff775485246999027b3197955", 18, Legacy13},
		{USDe, "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34", 18, StablesV1},
	}},
	{Chain: Base, Name: "Base", Icon: "base.png", ChainID: 8453, Tokens: []Token{
		{USDC, "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", 6, Legacy13},
		{USDT, "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2", 6, Legacy13},
	}},
	{Chain: Arbitrum, Name: "Arbitrum", Icon: "arbitrum.png", ChainID: 42161, Tokens: []Token{
		{USDC, "0xaf88d065e77c8cc2239327c5edb3a432268e5831", 6, Legacy13},
		{USDT, "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", 6, Legacy13},
		{USDe, "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34", 18, StablesV1},
	}},
	{Chain: Optimism, Name: "Optimism", ChainID: 10, Tokens: []Token{
		{USDC, "0x0b2c639c533813f4aa9d7837caf62653d097ff85", 6, StablesV1},
		{USDT, "0x94b008aa00579c1307b0ef2c499ad98a8ce58e58", 6, StablesV1},
	}},
	{Chain: Polygon, Name: "Polygon", Icon: "polygon.png", ChainID: 137, Tokens: []Token{
		{USDC, "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", 6, Legacy13},
		{USDT, "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", 6, Legacy13},
	}},
	{Chain: Monad, Name: "Monad", ChainID: 143, Tokens: []Token{
		{USDC, "0x754704bc059f8c67012fed69bc8a327a5aafb603", 6, StablesV1},
		{USDT, "0xe7cd86e13ac4309349f30b3435a9d337750fc82d", 6, StablesV1},
	}},
	{Chain: Tempo, Name: "Tempo", ChainID: 4217, Tokens: []Token{
		{USDC, "0x20c000000000000000000000b9537d11c60e8b50", 6, StablesV1},
		{USDT, "0x20c00000000000000000000014f22ca97301eb73", 6, StablesV1},
	}},
	{Chain: Robinhood, Name: "Robinhood Chain", ChainID: 4663, Tokens: []Token{
		{USDG, "0x5fc5360d0400a0fd4f2af552add042d716f1d168", 6, StablesV1},
	}},
	{Chain: MegaETH, Name: "MegaETH", ChainID: 4326, Tokens: []Token{
		{USDT, "0xb8ce59fc3717ada4c02eadf9682a9e934f625ebb", 6, StablesV1},
	}},
}

func Lookup(c Chain) (Network, bool) {
	for _, n := range Networks {
		if n.Chain == c {
			return n, true
		}
	}
	return Network{}, false
}

// Every deposit lands as USDC on Solana. Relay names Solana by its own chain
// id, not an EVM one.
const (
	SolanaRelayChainID = 792703809
	SolanaUSDCMint     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)
