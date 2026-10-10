package backyard

import (
	"context"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Unsigned simulation output is a prospective exit-cost input, never current
// custody, a signed build, or a committed position. No account overrides exist.
type phase3KaminoProjection struct {
	Slot          int64              `json:"slot"`
	MessageSHA256 string             `json:"messageSha256"`
	UnitsConsumed uint64             `json:"unitsConsumed"`
	Accounts      []ConfirmedAccount `json:"accounts"`
}

func depositProjectionAddresses(route RuntimeRoute) []string {
	addresses := []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply, route.DebtCustody, budgetClockAddress}
	if selectorLane(route.Lane) || route.Lane == autoAUTOPYUSD.Lane {
		addresses = append(addresses, route.Kamino.Market)
	}
	return addresses
}

func simulatePhase3EntryProjection(ctx context.Context, c *chain.Client, message []byte, addresses []string, minimumSlot int64) (phase3KaminoProjection, error) {
	var projection phase3KaminoProjection
	if c == nil || minimumSlot <= 0 || len(addresses) == 0 {
		return projection, budgetHold("invalid_deposit_projection_request")
	}
	_, err := checkedUnsignedMessage(message)
	if err != nil {
		return projection, err
	}
	keys, err := publicKeys(addresses)
	if err != nil {
		return projection, err
	}
	wire := append([]byte{1}, make([]byte, 64)...)
	wire = append(wire, message...)
	minimum := uint64(minimumSlot)
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimum, Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: keys}})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		return projection, budgetHold("deposit_projection_failed")
	}
	if err != nil {
		return projection, budgetHold("deposit_projection_unavailable")
	}
	slot := int64(simulated.Slot)
	if slot-minimumSlot > observationLagSlots() || simulated.Units == 0 {
		return projection, budgetHold("deposit_projection_failed")
	}
	// Like every RPC read a plan relies on, it carries the slot it was floored at.
	projection.Slot, projection.MessageSHA256, projection.UnitsConsumed = minimumSlot, sha256Bytes(message), simulated.Units
	for i, a := range simulated.Accounts {
		if a == nil || a.Executable {
			return projection, budgetHold("deposit_projection_incomplete")
		}
		projection.Accounts = append(projection.Accounts, ConfirmedAccount{Address: addresses[i], Owner: a.Owner.String(), Lamports: a.Lamports, Data: a.Data})
	}
	return projection, nil
}
