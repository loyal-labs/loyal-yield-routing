package earnprep

import (
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
)

type WithdrawProductAsset struct {
	Mint         solana.PublicKey
	Symbol       string
	TokenProgram solana.PublicKey
}

func WithdrawProductAssets(cluster string) []WithdrawProductAsset {
	assets := withdrawProductAssets(cluster)
	out := make([]WithdrawProductAsset, len(assets))
	for i, asset := range assets {
		out[i] = WithdrawProductAsset{Mint: asset.Mint, Symbol: asset.Symbol, TokenProgram: asset.TokenProgram}
	}
	return out
}

func WithdrawResolveProductAsset(cluster string, mint solana.PublicKey) (WithdrawProductAsset, error) {
	for _, asset := range withdrawProductAssets(cluster) {
		if asset.Mint == mint {
			return WithdrawProductAsset{Mint: asset.Mint, Symbol: asset.Symbol, TokenProgram: asset.TokenProgram}, nil
		}
	}
	return WithdrawProductAsset{}, fmt.Errorf("mint is not a supported Earn product mint.")
}

// WithdrawCanonicalUniverse: the full-exit proof always reads with the
// canonical product mints and the Safe basket, never with request metadata,
// because the canonical withdrawal input carries none.
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
		VaultIndex:           EarnVaultIndex,
		StableMints:          mints,
		KaminoLiquidityMints: mints,
		KaminoMarkets:        markets,
	}
}
