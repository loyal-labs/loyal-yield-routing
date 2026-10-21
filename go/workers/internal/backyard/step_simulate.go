package backyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

type SimulatedStep string

const (
	SimulateUp   SimulatedStep = "up"
	SimulateDown SimulatedStep = "down"
)

type StepAmount interface {
	simulated() (SimulatedStep, StepKind, uint64)
}

type UpBorrow struct{ DebtRaw uint64 }

type DownWithdraw struct{ CollateralReceiptRaw uint64 }

func (a UpBorrow) simulated() (SimulatedStep, StepKind, uint64) { return SimulateUp, stepUp, a.DebtRaw }

func (a DownWithdraw) simulated() (SimulatedStep, StepKind, uint64) {
	return SimulateDown, stepDown, a.CollateralReceiptRaw
}

// RunSimulateStep is read-only: it never signs or sends.
func RunSimulateStep(ctx context.Context, out io.Writer, rpcURL string, client *jupiter.Client, vault, lane string, amount StepAmount) error {
	direction, kind, amountRaw := amount.simulated()
	v, err := findVault(vault)
	if err != nil {
		return err
	}
	route, err := v.route(lane)
	if err != nil || !earnHeldLane(lane) {
		return fmt.Errorf("lane %q is no earn lane", lane)
	}
	if amountRaw == 0 || amountRaw > math.MaxInt64 {
		return fmt.Errorf("amount must be a positive raw amount")
	}
	manifest := v.manifest
	cluster, err := chain.New(rpcURL, 15*time.Second)
	if err != nil {
		return err
	}
	view, err := OpenView(ctx, cluster, nil)
	if err != nil {
		return err
	}
	viewSlot, err := view.slot(ctx)
	if err != nil {
		return err
	}
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, v, cluster, route, viewSlot)
	if err != nil {
		return err
	}
	st, err := readStepState(v, accounts, route, slot)
	if err != nil {
		return err
	}
	ltvBefore, err := observedLTVBPS(position)
	if err != nil {
		return err
	}
	policies, err := observeInstalledPolicies(ctx, cluster, v, viewSlot)
	if err != nil {
		return err
	}
	convert := amountRaw + st.heldDebt()
	if kind == stepDown {
		liquidity, err := st.collateral.redeemLiquidityRaw(amountRaw)
		if err != nil {
			return err
		}
		convert = liquidity + st.collateralBalance
	}
	evidence, guaranteed, err := assembleStep(ctx, cluster, view, client, manifest, policies, st, kind, amountRaw, convert, viewSlot)
	if err != nil {
		return err
	}
	step, err := buildStepMessage(v, evidence.Request, mustKey(backyardDelegate))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "lane: %s\ndir: %s\nobserved slot: %d\nLTV before: %d bps\n", lane, direction, slot, ltvBefore)
	fmt.Fprintf(out, "open: %d raw\nconvert: %d raw in, %d raw guaranteed out\nclose: %d raw\n", amountRaw, convert, guaranteed, evidence.Request.Close.AmountRaw)
	fmt.Fprintf(out, "v1: %d of %d bytes, %d of %d keys\n", step.WireBytes, maxTransactionBytes, step.Keys, maxTransactionKeys)
	wire := simulationWire(step.Message)
	capture := []solana.PublicKey{solanaKey(route.Kamino.Obligation), solanaKey(route.Kamino.CollateralReserve), solanaKey(route.Kamino.DebtReserve)}
	simulated, err := cluster.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, ReplaceRecentBlockhash: true,
		Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: capture}})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		fmt.Fprintf(out, "simulation: FAILED at slot %d: %v\nlogs tail:\n", failure.Slot, failure.Err)
		logs := failure.Logs
		if len(logs) > 20 {
			logs = logs[len(logs)-20:]
		}
		for _, line := range logs {
			fmt.Fprintf(out, "  %s\n", line)
		}
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "simulation: OK at slot %d\nCU: %d\n", simulated.Slot, simulated.Units)
	after := make([]ConfirmedAccount, len(capture))
	for i, account := range simulated.Accounts {
		if account == nil {
			return fmt.Errorf("simulation returned no %s", capture[i])
		}
		after[i] = confirmedAccount(capture[i].String(), account)
	}
	ltvAfter, err := stepLTVBPS(stepLTVAccounts{obligation: after[0], collateralReserve: after[1], debtReserve: after[2]}, route)
	if err != nil {
		return fmt.Errorf("LTV after: %w", err)
	}
	_, err = fmt.Fprintf(out, "LTV after: %d bps\n", ltvAfter)
	return err
}

type stepLTVAccounts struct {
	obligation, collateralReserve, debtReserve ConfirmedAccount
}

func stepLTVBPS(accounts stepLTVAccounts, route RuntimeRoute) (int64, error) {
	obligation, err := decodeKaminoObligation(accounts.obligation, route.Kamino)
	if err != nil {
		return 0, err
	}
	collateral, err := decodeKaminoReserve(accounts.collateralReserve, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	debt, err := decodeKaminoReserve(accounts.debtReserve, route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	redeemable, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return 0, err
	}
	debtRaw, err := obligation.debtAtReserveRate(debt)
	if err != nil {
		return 0, err
	}
	return observedLTVBPS(KaminoPosition{RedeemablePrimeRaw: redeemable, DebtRaw: debtRaw, CollateralPriceSF: collateral.terms.MarketPriceSF,
		DebtPriceSF: debt.terms.MarketPriceSF, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals})
}
