// Package bridge moves a user's deposits on EVM chains to their Solana wallet.
package bridge

import "fmt"

type Chain string

// Chain values are Privy's deposit-account chain names; Privy spells BNB
// Chain "bsc".
const (
	Ethereum Chain = "ethereum"
	Base     Chain = "base"
	Arbitrum Chain = "arbitrum"
	Polygon  Chain = "polygon"
	BNB      Chain = "bsc"
)

type Asset string

const (
	USDC Asset = "usdc"
	USDT Asset = "usdt"
	ETH  Asset = "eth"
)

// NativeToken is the address Privy and Relay use for a chain's own coin.
const NativeToken = "0x0000000000000000000000000000000000000000"

type Token struct {
	Asset    Asset
	Address  string
	Decimals uint8
}

type Network struct {
	Chain   Chain
	ChainID int64
	// MinUSD is the smallest deposit worth bridging; Ethereum gas makes
	// small transfers cost more than they move.
	MinUSD int
	Tokens []Token
}

func (n Network) CAIP2() string { return fmt.Sprintf("eip155:%d", n.ChainID) }

func (n Network) Token(a Asset) (Token, bool) {
	for _, t := range n.Tokens {
		if t.Asset == a {
			return t, true
		}
	}
	return Token{}, false
}

// Networks is the one list of chains and tokens Loyal accepts deposits on,
// copied from Privy's deposit-account config. BNB Chain's USDC and USDT use
// 18 decimals.
var Networks = []Network{
	{Chain: Ethereum, ChainID: 1, MinUSD: 20, Tokens: []Token{
		{USDC, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", 6},
		{USDT, "0xdac17f958d2ee523a2206206994597c13d831ec7", 6},
		{ETH, NativeToken, 18},
	}},
	{Chain: Base, ChainID: 8453, Tokens: []Token{
		{USDC, "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", 6},
		{USDT, "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2", 6},
		{ETH, NativeToken, 18},
	}},
	{Chain: Arbitrum, ChainID: 42161, Tokens: []Token{
		{USDC, "0xaf88d065e77c8cc2239327c5edb3a432268e5831", 6},
		{USDT, "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", 6},
		{ETH, NativeToken, 18},
	}},
	{Chain: Polygon, ChainID: 137, Tokens: []Token{
		{USDC, "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", 6},
		{USDT, "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", 6},
	}},
	{Chain: BNB, ChainID: 56, Tokens: []Token{
		{USDC, "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", 18},
		{USDT, "0x55d398326f99059ff775485246999027b3197955", 18},
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
