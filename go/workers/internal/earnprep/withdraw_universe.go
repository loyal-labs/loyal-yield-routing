package earnprep

import (
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
)

// Exported view of the product-mint universe the withdraw confirm route
// (earn-full-exit-zero-proof.server.ts) reads through: the full-exit proof
// builds one vault ATA per product asset and resolves the withdrawal mint's
// token program, and the Go confirm lives in internal/webapp.

// WithdrawProductAsset mirrors EarnProductAsset (earn-product-mints.shared.ts:
// 21) for consumers outside this package.
type WithdrawProductAsset struct {
	Mint         solana.PublicKey
	Symbol       string
	TokenProgram solana.PublicKey
}

// WithdrawProductAssets mirrors getEarnProductAssetsForCluster
// (earn-product-mints.shared.ts:29): the six canonical stablecoins in policy
// order on mainnet, the devnet USDC mint only elsewhere.
func WithdrawProductAssets(cluster string) []WithdrawProductAsset {
	assets := withdrawProductAssets(cluster)
	out := make([]WithdrawProductAsset, len(assets))
	for i, asset := range assets {
		out[i] = WithdrawProductAsset{Mint: asset.Mint, Symbol: asset.Symbol, TokenProgram: asset.TokenProgram}
	}
	return out
}

// WithdrawResolveProductAsset mirrors resolveEarnProductAsset
// (earn-product-mints.shared.ts:44): the product asset for a mint, or
// "mint is not a supported Earn product mint." for anything else.
func WithdrawResolveProductAsset(cluster string, mint solana.PublicKey) (WithdrawProductAsset, error) {
	for _, asset := range withdrawProductAssets(cluster) {
		if asset.Mint == mint {
			return WithdrawProductAsset{Mint: asset.Mint, Symbol: asset.Symbol, TokenProgram: asset.TokenProgram}, nil
		}
	}
	return WithdrawProductAsset{}, fmt.Errorf("mint is not a supported Earn product mint.")
}

// WithdrawCanonicalUniverse mirrors resolveFullExitPolicyMetadata
// (earn-full-exit-zero-proof.server.ts:58-75): the full-exit proof always
// reads with the canonical product mints and the Safe basket, never with
// request metadata, because the canonical withdrawal input carries none.
func WithdrawCanonicalUniverse(cluster string) *WithdrawPolicyUniverse {
	assets := withdrawProductAssets(cluster)
	mints := make([]string, 0, len(assets))
	for _, asset := range assets {
		mints = append(mints, asset.Mint.String())
	}
	safe := withdrawSafeMarkets(cluster)
	markets := make([]string, 0, len(safe))
	for _, market := range safe {
		markets = append(markets, market.String())
	}
	return &WithdrawPolicyUniverse{
		VaultIndex:           WithdrawVaultIndex,
		StableMints:          mints,
		KaminoLiquidityMints: mints,
		KaminoMarkets:        markets,
	}
}
