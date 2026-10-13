package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/solana-foundation/solana-go/v2/rpc"
)

// observeConfirmedReserveCatalog reads the complete reserve catalog once at
// confirmed commitment from a node at or past minimumSlot. Any invalid reserve
// fails the whole observation. The caller still verifies durable hashes,
// identities and expiry afterward.
func (w *Worker) observeConfirmedReserveCatalog(ctx context.Context, addresses []string, identities map[string]ReserveIdentity, minimumSlot int64) (MarketSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	slotsPerSecond, err := w.rpc.SlotsPerSecond(ctx)
	if err != nil {
		return MarketSnapshot{}, fmt.Errorf("measured slot rate: %w", err)
	}
	slot, accounts, err := ReadAccounts(ctx, w.rpc, addresses, rpc.CommitmentConfirmed, minimumSlot)
	if err != nil {
		return MarketSnapshot{}, err
	}
	direct := MarketSnapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: make(map[string]ReserveState, len(accounts))}
	for i, account := range accounts {
		state, err := DecodeKaminoSourceReserve(account, identities[addresses[i]], slot, slotsPerSecond)
		if err != nil {
			return MarketSnapshot{}, err
		}
		direct.Reserves[addresses[i]] = state
	}
	return direct, nil
}
