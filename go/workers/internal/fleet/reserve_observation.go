package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// observeConfirmedReserveCatalog retries only inconsistent slot ordering in an
// otherwise readable RPC batch. Each attempt replaces the entire observation;
// no account bytes or economics from a rejected batch survive into planning.
// The caller still verifies durable hashes, identities and expiry afterward.
func (w *Worker) observeConfirmedReserveCatalog(ctx context.Context, addresses []string, identities map[string]ReserveIdentity, minimumSlot int64) (MarketSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		slot, accounts, err := w.rpc.ConfirmedAccounts(ctx, addresses, minimumSlot)
		if err != nil {
			return MarketSnapshot{}, err
		}
		direct := MarketSnapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: make(map[string]ReserveState, len(accounts))}
		var mismatch *ReserveSlotOrderMismatch
		nextMinimumSlot := slot
		for i, account := range accounts {
			state, err := DecodeKaminoSourceReserve(account, identities[addresses[i]], slot, w.config.SlotDuration)
			if err != nil {
				var order *ReserveSlotOrderMismatch
				if !errors.As(err, &order) {
					return MarketSnapshot{}, err
				}
				mismatch = order
				if order.LastUpdateSlot > nextMinimumSlot {
					nextMinimumSlot = order.LastUpdateSlot
				}
				continue
			}
			direct.Reserves[addresses[i]] = state
		}
		if err := ctx.Err(); err != nil {
			return MarketSnapshot{}, err
		}
		if mismatch == nil {
			return direct, nil
		}
		if attempt == attempts {
			return MarketSnapshot{}, fmt.Errorf("reserve catalog remains incoherent after %d observations: %w", attempts, mismatch)
		}
		// RPC validation guarantees slot >= the previous minimum. Raising this
		// floor never relaxes confirmation or silently repairs the response slot.
		minimumSlot = nextMinimumSlot
		logEvent(map[string]any{"event": "kamino_fleet_planner_observation_retry", "reason": "reserve_update_ahead_of_rpc_context", "attempt": attempt, "contextSlot": slot, "minimumSlot": minimumSlot})
		timer := time.NewTimer(time.Duration(attempt) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return MarketSnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable reserve observation attempt")
}
