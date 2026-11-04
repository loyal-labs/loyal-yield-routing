package bridge

import (
	"fmt"
	"strings"
)

type AddressKind string

const (
	AddressEVM  AddressKind = "evm"
	AddressTron AddressKind = "tron"
)

type DestinationToken struct {
	Asset    Asset
	Address  string
	Decimals uint8
}

type Destination struct {
	Chain        Chain
	RelayChainID int64
	Name         string
	Icon         string
	Kind         AddressKind
	ExplorerTx   string
	PopularRank  int
	Tokens       []DestinationToken
}

var Destinations = []Destination{
	{Chain: Ethereum, PopularRank: 1, RelayChainID: 1, Name: "Ethereum (ERC20)", Icon: "ethereum.png", Kind: AddressEVM, ExplorerTx: "https://etherscan.io/tx/", Tokens: []DestinationToken{
		{USDC, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", 6},
		{USDT, "0xdac17f958d2ee523a2206206994597c13d831ec7", 6},
		{PYUSD, "0x6c3ea9036406852006290770bedfcaba0e23a0e8", 6},
		{USDG, "0xe343167631d89b6ffc58b88d6b7fb0228795491d", 6},
		{DAI, "0x6b175474e89094c44da98b954eedeac495271d0f", 18},
		{USDe, "0x4c9edd5852cd905f086c759e8383e09bff1e68b3", 18},
		{USDS, "0xdc035d45d973e3ec169d2276ddab16f1e407384f", 18},
		{FDUSD, "0xc5f0f7b66764f6ec8c8dff7ba683102295e16409", 18},
		{RLUSD, "0x8292bb45bf1ee4d140127049757c2e0ff06317ed", 18},
		{USD1, "0x8d0d000ee44948fc98c9b98a4fa4921476f08b0d", 18},
	}},
	{Chain: BNB, RelayChainID: 56, Name: "BNB Smart Chain (BEP20)", Icon: "bnb.png", Kind: AddressEVM, ExplorerTx: "https://bscscan.com/tx/", Tokens: []DestinationToken{
		{USDC, "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", 18},
		{USDT, "0x55d398326f99059ff775485246999027b3197955", 18},
		{USDe, "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34", 18},
		{DAI, "0x1af3f329e8be154074d8769d1ffa4ee058b1dbc3", 18},
		{FDUSD, "0xc5f0f7b66764f6ec8c8dff7ba683102295e16409", 18},
		{USD1, "0x8d0d000ee44948fc98c9b98a4fa4921476f08b0d", 18},
	}},
	{Chain: Base, PopularRank: 2, RelayChainID: 8453, Name: "Base Mainnet", Icon: "base.png", Kind: AddressEVM, ExplorerTx: "https://basescan.org/tx/", Tokens: []DestinationToken{
		{USDC, "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", 6},
		{USDT, "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2", 6},
		{USDS, "0x820c137fa70c8691f0e44dc420a5e53c168921dc", 18},
		{DAI, "0x50c5725949a6f0c72e6c4a641f24049a917db0cb", 18},
		{USDe, "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34", 18},
		{RLUSD, "0x8d58c0c60b8d6b88fa98b291a646db34d0f98258", 18},
	}},
	{Chain: Arbitrum, PopularRank: 3, RelayChainID: 42161, Name: "Arbitrum One", Icon: "arbitrum.png", Kind: AddressEVM, ExplorerTx: "https://arbiscan.io/tx/", Tokens: []DestinationToken{
		{USDC, "0xaf88d065e77c8cc2239327c5edb3a432268e5831", 6},
		{USDT, "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", 6},
		{USDe, "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34", 18},
		{PYUSD, "0x46850ad61c2b7d64d08c9c754f45254596696984", 6},
		{USDG, "0x004b506865409877c9fa29bfb1eba929984b9bbc", 6},
		{DAI, "0xda10009cbd5d07dd0cecc66161fc93d7c9000da1", 18},
	}},
	{Chain: Optimism, RelayChainID: 10, Name: "OP Mainnet", Icon: "", Kind: AddressEVM, ExplorerTx: "https://optimistic.etherscan.io/tx/", Tokens: []DestinationToken{
		{USDC, "0x0b2c639c533813f4aa9d7837caf62653d097ff85", 6},
		{USDT, "0x94b008aa00579c1307b0ef2c499ad98a8ce58e58", 6},
		{USDT0, "0x01bff41798a0bcf287b996046ca68b395dbc1071", 6},
		{DAI, "0xda10009cbd5d07dd0cecc66161fc93d7c9000da1", 18},
	}},
	{Chain: Polygon, PopularRank: 5, RelayChainID: 137, Name: "Polygon PoS", Icon: "polygon.png", Kind: AddressEVM, ExplorerTx: "https://polygonscan.com/tx/", Tokens: []DestinationToken{
		{USDC, "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", 6},
		{USDT, "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", 6},
		{DAI, "0x8f3cf7ad23cd3cadbd9735aff958023239c6a063", 18},
	}},
	{Chain: Monad, RelayChainID: 143, Name: "Monad", Icon: "", Kind: AddressEVM, ExplorerTx: "https://monadvision.com/tx/", Tokens: []DestinationToken{
		{USDC, "0x754704bc059f8c67012fed69bc8a327a5aafb603", 6},
		{USDT, "0xe7cd86e13ac4309349f30b3435a9d337750fc82d", 6},
	}},
	{Chain: Tempo, RelayChainID: 4217, Name: "Tempo", Icon: "", Kind: AddressEVM, ExplorerTx: "https://explore.tempo.xyz/tx/", Tokens: []DestinationToken{
		{USDCe, "0x20c000000000000000000000b9537d11c60e8b50", 6},
		{USDT, "0x20c00000000000000000000014f22ca97301eb73", 6},
	}},
	{Chain: Robinhood, RelayChainID: 4663, Name: "Robinhood Chain", Icon: "", Kind: AddressEVM, ExplorerTx: "https://robin.etherscan.io/tx/", Tokens: []DestinationToken{
		{USDG, "0x5fc5360d0400a0fd4f2af552add042d716f1d168", 6},
	}},
	{Chain: MegaETH, RelayChainID: 4326, Name: "MegaETH", Icon: "", Kind: AddressEVM, ExplorerTx: "https://megaeth.blockscout.com/tx/", Tokens: []DestinationToken{
		{USDT, "0xb8ce59fc3717ada4c02eadf9682a9e934f625ebb", 6},
	}},
	{Chain: Avalanche, PopularRank: 6, RelayChainID: 43114, Name: "Avalanche C-Chain", Icon: "avalanche.png", Kind: AddressEVM, ExplorerTx: "https://snowtrace.io/tx/", Tokens: []DestinationToken{
		{USDC, "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", 6},
		{USDT, "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", 6},
	}},
	{Chain: Plasma, RelayChainID: 9745, Name: "Plasma", Icon: "", Kind: AddressEVM, ExplorerTx: "https://plasmascan.to/tx/", Tokens: []DestinationToken{
		{USDT0, "0xb8ce59fc3717ada4c02eadf9682a9e934f625ebb", 6},
	}},
	{Chain: "tron", PopularRank: 4, RelayChainID: 728126428, Name: "Tron (TRC20)", Icon: "tron.png", Kind: AddressTron, ExplorerTx: "https://tronscan.org/#/transaction/", Tokens: []DestinationToken{
		{USDT, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", 6},
	}},
}

func LookupDestination(c Chain) (Destination, bool) {
	for _, d := range Destinations {
		if d.Chain == c {
			return d, true
		}
	}
	return Destination{}, false
}

func (d Destination) Token(a Asset) (DestinationToken, bool) {
	for _, t := range d.Tokens {
		if t.Asset == a {
			return t, true
		}
	}
	return DestinationToken{}, false
}

func init() {
	seen := map[Chain]bool{}
	for _, d := range Destinations {
		if seen[d.Chain] {
			panic(fmt.Sprintf("bridge: %s is a destination twice", d.Chain))
		}
		seen[d.Chain] = true
		for _, t := range d.Tokens {
			if _, ok := Assets[t.Asset]; !ok {
				panic(fmt.Sprintf("bridge: %s on %s has no entry in Assets", t.Asset, d.Chain))
			}
			if net, ok := Lookup(d.Chain); ok {
				if inbound, ok := net.Token(t.Asset); ok && (!strings.EqualFold(inbound.Address, t.Address) || inbound.Decimals != t.Decimals || net.ChainID != d.RelayChainID) {
					panic(fmt.Sprintf("bridge: %s on %s differs from Networks", t.Asset, d.Chain))
				}
			}
			if d.Chain == Tron.Chain && (t.Asset != Tron.Asset || t.Address != Tron.Token || t.Decimals != Tron.Decimals || d.RelayChainID != Tron.RelayChainID) {
				panic(fmt.Sprintf("bridge: %s on tron differs from bridge.Tron", t.Asset))
			}
		}
	}
}
