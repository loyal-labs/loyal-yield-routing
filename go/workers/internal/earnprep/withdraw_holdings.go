package earnprep

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// The RPC holdings snapshot, a mirror of fetchEarnRpcHoldingsSnapshot
// (web/apps/web/src/lib/yield-optimization/earn-rpc-holdings.client.ts:422-662).
// Stage 1 reads the Earn vault's idle product ATAs and its vanilla obligation
// per allowed market; stage 2 reads the reserves behind every discovered
// deposit. Kamino amounts are redeemable liquidity, not raw collateral. The
// function only throws; callers map failures to their own envelopes.

// RPC is the chain surface the withdraw family reads through; *chain.Client
// satisfies it.
type RPC interface {
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
}

// WithdrawVaultIndex is the Earn vault's account index (EARN_VAULT_INDEX,
// loyal-actions/src/constants.ts:49).
const WithdrawVaultIndex uint8 = 1

// withdrawCommitment is SOURCE_COMMITMENT (earn-rpc-holdings.client.ts:44).
var withdrawCommitment = rpc.CommitmentConfirmed

const withdrawGetMultipleAccountsLimit = 100

// WithdrawPolicyUniverse is the policy metadata the snapshot narrows to
// (EarnRpcPolicyMetadata, earn-rpc-holdings.client.ts:47): the vault index,
// the policy's stable mints, its Kamino liquidity mints and its markets.
type WithdrawPolicyUniverse struct {
	VaultIndex           uint8
	StableMints          []string
	KaminoLiquidityMints []string
	KaminoMarkets        []string
}

// withdrawProductAsset mirrors EarnProductAsset (earn-product-mints.shared.ts:21).
// The token program follows the stablecoin, not the cluster
// (loyal-actions/src/constants.ts:253): CASH, USDG and PYUSD are Token-2022.
type withdrawProductAsset struct {
	Mint         solana.PublicKey
	Symbol       string
	TokenProgram solana.PublicKey
}

var (
	withdrawDevnetUSDCMint = solana.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	withdrawTokenProgram   = solana.TokenProgramID
	withdrawToken2022      = solana.Token2022ProgramID
)

// withdrawProductAssets mirrors getEarnProductAssetsForCluster
// (earn-product-mints.shared.ts:29): the six canonical stablecoins in policy
// order on mainnet, the devnet USDC mint only elsewhere.
func withdrawProductAssets(cluster string) []withdrawProductAsset {
	if cluster == "devnet" {
		return []withdrawProductAsset{{Mint: withdrawDevnetUSDCMint, Symbol: "USDC", TokenProgram: withdrawTokenProgram}}
	}
	return []withdrawProductAsset{
		{Mint: solana.MustPublicKeyFromBase58("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"), Symbol: "CASH", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"), Symbol: "USDG", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"), Symbol: "PYUSD", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), Symbol: "USDC", TokenProgram: withdrawTokenProgram},
		{Mint: solana.MustPublicKeyFromBase58("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"), Symbol: "USDT", TokenProgram: withdrawTokenProgram},
		{Mint: solana.MustPublicKeyFromBase58("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"), Symbol: "USDS", TokenProgram: withdrawTokenProgram},
	}
}

// withdrawSafeMarkets mirrors getRiskBasketMarketsForCluster(cluster, Safe)
// (loyal-actions/src/constants.ts:97-134).
func withdrawSafeMarkets(cluster string) []solana.PublicKey {
	if cluster == "devnet" {
		return []solana.PublicKey{solana.MustPublicKeyFromBase58("27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J")}
	}
	return []solana.PublicKey{
		solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
		solana.MustPublicKeyFromBase58("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
		solana.MustPublicKeyFromBase58("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
		solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
		solana.MustPublicKeyFromBase58("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"),
	}
}

// assertWithdrawPolicyUniverse mirrors assertPolicyUniverse
// (earn-rpc-holdings.client.ts:188): the product assets the policy lists in
// both its stable mints and its Kamino liquidity mints, and the policy's
// markets that are in the Safe basket, in policy order.
func assertWithdrawPolicyUniverse(cluster string, policy *WithdrawPolicyUniverse) ([]withdrawProductAsset, []solana.PublicKey, error) {
	if policy == nil {
		return nil, nil, errors.New("Active Earn policy metadata is required for RPC holdings.")
	}
	if policy.VaultIndex != WithdrawVaultIndex {
		return nil, nil, errors.New("Active Earn policy is not for the Earn vault.")
	}
	var assets []withdrawProductAsset
	for _, asset := range withdrawProductAssets(cluster) {
		mint := asset.Mint.String()
		if slices.Contains(policy.StableMints, mint) && slices.Contains(policy.KaminoLiquidityMints, mint) {
			assets = append(assets, asset)
		}
	}
	if len(assets) == 0 {
		return nil, nil, errors.New("Active Earn policy includes no supported product mint.")
	}
	var markets []solana.PublicKey
	for _, market := range policy.KaminoMarkets {
		key, err := solana.PublicKeyFromBase58(market)
		if err != nil {
			return nil, nil, err
		}
		if slices.ContainsFunc(withdrawSafeMarkets(cluster), func(safe solana.PublicKey) bool { return safe == key }) {
			markets = append(markets, key)
		}
	}
	if len(markets) == 0 {
		return nil, nil, errors.New("Active Earn policy has no Safe Kamino markets.")
	}
	return assets, markets, nil
}

// withdrawPositionDisplay mirrors resolveEarnPositionDisplay
// (earn-position-display.ts:81): known Safe-basket market names, known mint
// symbols, shortened addresses otherwise.
func withdrawPositionDisplay(liquidityMint string, market *solana.PublicKey) (label, marketName, mintSymbol string) {
	if market == nil {
		marketName = "Unknown Market"
	} else {
		marketName = withdrawKnownMarketName(market.String())
	}
	mintSymbol = withdrawKnownMintSymbol(liquidityMint)
	return marketName + " · " + mintSymbol, marketName, mintSymbol
}

func withdrawKnownMarketName(market string) string {
	switch market {
	case "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF", "27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J":
		return "Main Kamino"
	case "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA":
		return "Prime Market"
	case "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y":
		return "Maple Market"
	case "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8":
		return "OnRe Market"
	case "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF":
		return "Ethena Market"
	}
	return withdrawShortenAddress(market)
}

func withdrawKnownMintSymbol(mint string) string {
	for _, asset := range withdrawProductAssets("mainnet-beta") {
		if asset.Mint.String() == mint {
			return asset.Symbol
		}
	}
	if mint == withdrawDevnetUSDCMint.String() {
		return "USDC"
	}
	return withdrawShortenAddress(mint)
}

// withdrawShortenAddress mirrors shortenAddress
// (earn-position-display.ts:73): ten characters or fewer stay whole, anything
// longer is head 4 + "..." + tail 4.
func withdrawShortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:4] + "..." + address[len(address)-4:]
}

// withdrawObservedAt formats like Date.prototype.toISOString: millisecond
// precision with a Z suffix.
func withdrawObservedAt(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return now().UTC().Format("2006-01-02T15:04:05.000Z")
}
