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

// RunSimulateStep builds lane's atomic up or down step from the chain as it
// is now, through the production step assembly (assembleStep), with the
// delegate as fee payer, and simulates it unsigned (sigVerify off, the
// cluster's blockhash): the read-only proof that one transaction carries the
// whole step through the installed policies. It never signs or sends.
//
// up borrows amountRaw of debt, converts exactly that into collateral and
// deposits held collateral plus the conversion's guaranteed output. down
// withdraws amountRaw of collateral receipt (KLend's withdraw amount),
// converts the liquidity it redeems at the observed rate and the held
// collateral into debt, and repays held debt plus the conversion's
// guaranteed output.
func RunSimulateStep(ctx context.Context, out io.Writer, rpcURL string, client *jupiter.Client, lane string, up bool, amountRaw uint64) error {
	route, err := runtimeRoute(lane)
	if err != nil || !earnHeldLane(lane) {
		return fmt.Errorf("lane %q is no earn lane", lane)
	}
	if amountRaw == 0 || amountRaw > math.MaxInt64 {
		return fmt.Errorf("amount must be a positive raw amount")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	cluster, err := chain.New(rpcURL, 15*time.Second)
	if err != nil {
		return err
	}
	view, err := OpenView(ctx, cluster, nil)
	if err != nil {
		return err
	}
	// The view is its one start-up read, at viewSlot: later reads are floored
	// there, not at the refreshed batch's simulation slot past it.
	viewSlot, err := view.slot(ctx)
	if err != nil {
		return err
	}
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, cluster, route, viewSlot)
	if err != nil {
		return err
	}
	st, err := readStepState(accounts, route, slot)
	if err != nil {
		return err
	}
	ltvBefore, err := observedLTVBPS(position)
	if err != nil {
		return err
	}
	policies, err := observeInstalledPolicies(ctx, cluster, viewSlot)
	if err != nil {
		return err
	}
	convert := amountRaw + st.heldDebt
	if !up {
		liquidity, err := st.collateral.redeemLiquidityRaw(amountRaw)
		if err != nil {
			return err
		}
		convert = liquidity + st.heldCollateral
	}
	evidence, guaranteed, err := assembleStep(ctx, cluster, view, client, manifest, policies, st, up, amountRaw, convert, viewSlot)
	if err != nil {
		return err
	}
	step, err := buildStepMessage(evidence.Request, mustKey(bridgeDelegate))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "lane: %s\ndir: %s\nobserved slot: %d\nLTV before: %d bps\n", lane, map[bool]string{true: "up", false: "down"}[up], slot, ltvBefore)
	fmt.Fprintf(out, "open: %d raw\nconvert: %d raw in, %d raw guaranteed out\nclose: %d raw\n", amountRaw, convert, guaranteed, evidence.Request.Close.AmountRaw)
	fmt.Fprintf(out, "v1: %d of %d bytes, %d of %d keys\n", step.WireBytes, maxTransactionBytes, step.Keys, maxTransactionKeys)
	// An all-zero signature: with sigVerify off the cluster runs the message
	// as the delegate's without its key, and these bytes can never land.
	wire := signedWire(step.Message, make([]byte, 64))
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
	ltvAfter, err := stepLTVBPS(after, route)
	if err != nil {
		return fmt.Errorf("LTV after: %w", err)
	}
	_, err = fmt.Fprintf(out, "LTV after: %d bps\n", ltvAfter)
	return err
}

// stepLTVBPS is the obligation's LTV over the simulation's own refreshed
// reserves: accounts are the obligation, then its collateral and debt
// reserves.
func stepLTVBPS(accounts []ConfirmedAccount, route RuntimeRoute) (int64, error) {
	obligation, err := decodeKaminoObligation(accounts[0], route.Kamino)
	if err != nil {
		return 0, err
	}
	collateral, err := decodeKaminoReserve(accounts[1], route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	debt, err := decodeKaminoReserve(accounts[2], route.Kamino.DebtMint, route.Kamino)
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
	return observedLTVBPS(KaminoPosition{RedeemablePrimeRaw: redeemable, DebtRaw: debtRaw, CollateralPriceSF: collateral.marketPriceSF,
		DebtPriceSF: debt.marketPriceSF, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals})
}
