package worker

import (
	"context"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	solanago "github.com/solana-foundation/solana-go/v2"
)

var mainnetGenesis = solanago.MustHashFromBase58("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")

// The watched mint/market identities and Apps legacy namespace exception are
// mainnet-specific. Check the actual endpoint before opening writers and on
// each watch refresh; a configured cluster label alone is insufficient.
func validateWatchNamespace(ctx context.Context, cluster string, rpc *chain.Client) error {
	if cluster != "mainnet-beta" || rpc == nil {
		return errors.New("observer fixed watch catalog requires mainnet-beta namespace")
	}
	hash, err := rpc.GenesisHash(ctx)
	if err != nil {
		return err
	}
	if hash != mainnetGenesis {
		return errors.New("observer watch RPC is not Solana mainnet")
	}
	return nil
}
