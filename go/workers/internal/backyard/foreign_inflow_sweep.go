package backyard

import (
	"context"
	"fmt"
	"math"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func validateForeignSweep(v *Vault, e ExpectedEffects) error {
	if e.ForeignCustodyRaw == nil {
		return nil
	}
	if v.trustful() || e.Kind != effectsBridge || !e.Conserved || len(e.Accounts) != 3 {
		return fmt.Errorf("invalid foreign custody sweep")
	}
	var idle, strategy, cash *ExpectedAccountEffect
	for i := range e.Accounts {
		a := &e.Accounts[i]
		if a.BeforeRaw > math.MaxInt64 || a.AfterRaw > math.MaxInt64 || a.MinimumAfterRaw != nil {
			return fmt.Errorf("invalid foreign sweep amount")
		}
		switch a.Address {
		case v.idleATA:
			idle = a
		case v.strategyATA:
			strategy = a
		case v.cash:
			cash = a
		}
	}
	if idle == nil || strategy == nil || cash == nil || strategy.BeforeRaw == 0 || strategy.AfterRaw != 0 ||
		idle.AfterRaw < idle.BeforeRaw || idle.AfterRaw-idle.BeforeRaw != strategy.BeforeRaw || cash.AfterRaw != cash.BeforeRaw ||
		*e.ForeignCustodyRaw > strategy.BeforeRaw || strategy.BeforeRaw-*e.ForeignCustodyRaw > strategyTwoBridgeLegCapRaw {
		return fmt.Errorf("foreign sweep must restore exact staged principal plus proven gifts")
	}
	return nil
}

// A gift can land after the signed restore was planned. The old expected
// principal stays fixed; only an independently replayed foreign credit can grow.
func proveForeignSweep(ctx context.Context, db foreignInflowStore, c foreignInflowChain, v *Vault, e ExpectedEffects, receipt *ConfirmedTransactionEvidence) error {
	if e.ForeignCustodyRaw == nil {
		return nil
	}
	if err := validateForeignSweep(v, e); err != nil {
		return err
	}
	var planned uint64
	for _, a := range e.Accounts {
		if a.Address == v.strategyATA {
			planned = a.BeforeRaw
		}
	}
	var actual uint64
	for _, a := range receipt.PreTokenBalances {
		if a.Address == v.strategyATA {
			actual = a.Raw
		}
	}
	if actual <= planned {
		return nil
	}
	before, err := replayForeignInflows(ctx, db, c, v, v.strategyATA, uint64(receipt.Slot), uint64(receipt.Slot), receipt.Signature)
	if err != nil {
		return err
	}
	if before.BalanceRaw != actual || before.ForeignRaw < *e.ForeignCustodyRaw || before.ForeignRaw-*e.ForeignCustodyRaw != actual-planned {
		return foreignAccountingFault("restore surplus is not explained by foreign inflows")
	}
	x, err := c.Execution(ctx, solana.MustSignatureFromBase58(receipt.Signature), rpc.CommitmentFinalized)
	if err != nil {
		return err
	}
	evidence, err := advanceForeignCheckpoint(v, v.strategyATA, receipt.Signature, before, x)
	if err != nil {
		return err
	}
	if x.Slot != uint64(receipt.Slot) || evidence.After.BalanceRaw != 0 || evidence.After.ForeignRaw != 0 {
		return fmt.Errorf("foreign sweep did not empty custody")
	}
	receipt.foreignSweepExtraRaw = actual - planned
	receipt.foreignSweepCheckpoint = before
	return nil
}
