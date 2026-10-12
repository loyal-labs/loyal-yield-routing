package backyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// RunSimulateStep builds lane's atomic up or down step from the chain as it
// is now, with the delegate as fee payer, and simulates it unsigned
// (sigVerify off, the cluster's blockhash): the read-only proof that one
// transaction carries the whole step through the installed policies. It
// never signs or sends.
//
// up borrows amountRaw of debt, converts exactly that into collateral and
// deposits held collateral plus the conversion's guaranteed output. down
// withdraws amountRaw of collateral receipt (KLend's withdraw amount),
// converts the liquidity it redeems at the observed rate into debt and repays
// held debt plus the conversion's guaranteed output. tables name lookup
// tables to offer beside the route valuation table and the swap's own.
func RunSimulateStep(ctx context.Context, out io.Writer, rpcURL string, client *jupiter.Client, lane string, up bool, amountRaw uint64, tables []string) error {
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
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, cluster, view, route, viewSlot)
	if err != nil {
		return err
	}
	heldCollateral, err := stepCustodyRaw(accounts, route.CollateralCustody, route.Kamino.CollateralMint)
	if err != nil {
		return err
	}
	heldDebt, err := stepCustodyRaw(accounts, route.DebtCustody, route.Kamino.DebtMint)
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
	blockhash, err := latestBlockhash(ctx, cluster)
	if err != nil {
		return err
	}
	// Open's refresh runs over the obligation as it is: its deposit, then
	// its debt.
	var reserves []string
	if position.CollateralDepositedRaw > 0 {
		reserves = append(reserves, route.Kamino.CollateralReserve)
	}
	if position.DebtRaw > 0 {
		reserves = append(reserves, route.Kamino.DebtReserve)
	}
	kaminoAction, openLeg, closeLeg, conversion := DeleverRouteStep, kaminoLegWithdraw, kaminoLegRepay, SwapCollateralToDebtStep
	convertRaw, sourceRaw, destinationRaw := uint64(0), heldCollateral, heldDebt
	if up {
		if position.CollateralDepositedRaw == 0 {
			return fmt.Errorf("%s has no collateral deposited to borrow against", lane)
		}
		kaminoAction, openLeg, closeLeg, conversion = OpenRouteStep, kaminoLegBorrow, kaminoLegDeposit, SwapDebtToCollateralStep
		convertRaw, sourceRaw, destinationRaw = amountRaw, heldDebt, heldCollateral
	} else {
		if position.DebtRaw == 0 {
			return fmt.Errorf("%s has no debt to repay", lane)
		}
		reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if err != nil {
			return err
		}
		if convertRaw, err = reserve.redeemLiquidityRaw(amountRaw); err != nil {
			return err
		}
	}
	if sourceRaw > math.MaxUint64-convertRaw || convertRaw > math.MaxInt64 {
		return fmt.Errorf("step source custody overflows")
	}
	evidence, err := prepareConversionQuoteEvidence(ctx, cluster, view, client, manifest, policies,
		Decision{Action: conversion, AmountRaw: int64(convertRaw), StrategyKey: lane}, sourceRaw+convertRaw, destinationRaw, viewSlot)
	if err != nil {
		return err
	}
	guaranteed, err := stepConversionOutput(evidence.Request)
	if err != nil {
		return err
	}
	closeRaw, err := stepCloseAmount(destinationRaw, evidence.Request)
	if err != nil {
		return err
	}
	open, err := manifest.kaminoPacketForRoute(policies, kaminoAction, openLeg, amountRaw, blockhash, lane)
	if err != nil {
		return err
	}
	open.ObligationReserves = reserves
	closing, err := manifest.kaminoPacketForRoute(policies, kaminoAction, closeLeg, closeRaw, blockhash, lane)
	if err != nil {
		return err
	}
	// A key loads through the first offered table holding it: the valuation
	// table first (it holds most of a lane), then the operator's, then the
	// swap's own.
	candidates := append([]string{routeValuationLookupTable}, tables...)
	if swap, ok := evidence.Request.(JupiterSwapRequest); ok {
		candidates = append(candidates, swap.Instruction.LookupTableAddresses...)
	}
	var offered []string
	for _, address := range candidates {
		if !slices.Contains(offered, address) {
			offered = append(offered, address)
		}
	}
	snapshots, err := observeJupiterLookupTables(ctx, cluster, offered, viewSlot)
	if err != nil {
		return err
	}
	step, err := buildStepMessage(atomicStep{Open: open, Conversion: evidence.Request, Close: closing, Tables: snapshots}, mustKey(bridgeDelegate))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "lane: %s\ndir: %s\nobserved slot: %d\nLTV before: %d bps\n", lane, map[bool]string{true: "up", false: "down"}[up], slot, ltvBefore)
	fmt.Fprintf(out, "open %s: %d raw\nconvert %s: %d raw in, %d raw guaranteed out\nclose %s: %d raw (held %d + guaranteed out)\n",
		openLeg, amountRaw, conversion, convertRaw, guaranteed, closeLeg, closeRaw, destinationRaw)
	verdict := fmt.Sprintf("fits, %d spare", solanaPacketBytes-step.WireBytes)
	if !step.Fits() {
		verdict = fmt.Sprintf("OVER by %d", step.WireBytes-solanaPacketBytes)
	}
	fmt.Fprintf(out, "bytes: %d of %d (%s)\naccounts: %d (%d static)\nALTs offered: %s\nALTs used: %s\n",
		step.WireBytes, solanaPacketBytes, verdict, step.Accounts, step.StaticAccounts, strings.Join(offered, " "), strings.Join(step.Tables, " "))
	// An all-zero signature: with sigVerify off the cluster runs the message
	// as the delegate's without its key, and these bytes can never land.
	wire := append(append([]byte{1}, make([]byte, 64)...), step.Message...)
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

// stepCustodyRaw is the vault's balance in its custody of mint.
func stepCustodyRaw(accounts []ConfirmedAccount, address, mint string) (uint64, error) {
	mintKey, err := decodeBase58PublicKey(mint)
	if err != nil {
		return 0, err
	}
	authority, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return 0, err
	}
	account := accountAt(accounts, address)
	custody, err := DecodeTokenCustody(account.Owner, account.Data, mintKey, authority)
	if err != nil {
		return 0, fmt.Errorf("custody %s: %w", address, err)
	}
	return custody.Raw, nil
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
