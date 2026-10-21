package backyard

import (
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type KaminoPrimeUSDCAccounts []struct {
	Address  string
	Signer   bool
	Writable bool
}

type KaminoLegRequest struct {
	Action               Action
	AmountRaw            uint64
	Policy               string
	Accounts             KaminoPrimeUSDCAccounts
	Data                 []byte
	RecentBlockhash      string
	LastValidBlockHeight int64
	RouteLane            string
	ObligationReserves   []string
}

// obligationTopology is what RefreshObligation names after the obligation:
// the reserve of each occupied deposit slot, then of each occupied borrow
// slot, in slot order, as KLend counts and checks them (none for an empty
// obligation).
func obligationTopology(o kamino.Obligation) []string {
	topology := []string{}
	for _, reserve := range append(o.DepositReserves(), o.BorrowReserves()...) {
		topology = append(topology, reserve.String())
	}
	return topology
}

func observedObligationTopology(accounts []ConfirmedAccount, route RuntimeRoute) ([]string, error) {
	account := accountAt(accounts, route.Kamino.Obligation)
	if account.Address != route.Kamino.Obligation {
		return nil, fmt.Errorf("Kamino obligation %s is not in the batch", route.Kamino.Obligation)
	}
	if account.Lamports == 0 {
		return []string{}, nil
	}
	obligation, err := kamino.DecodeObligation(chainAccount(account, route.Kamino.Obligation))
	if err != nil {
		return nil, err
	}
	return obligationTopology(obligation), nil
}

const (
	kaminoScopePrices  = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"
	kaminoInstructions = "Sysvar1nstructions1111111111111111111111111"
)

type kaminoPrimeUSDCLeg byte

const (
	kaminoLegDeposit kaminoPrimeUSDCLeg = iota + 1
	kaminoLegBorrow
	kaminoLegRepay
	kaminoLegWithdraw
)

func compileKaminoMessageForDelegate(v *Vault, request KaminoLegRequest, delegate publicKey) ([]byte, error) {
	route, err := v.route(request.RouteLane)
	if err != nil {
		return nil, err
	}
	return compileReviewedKaminoMessage(v, request, delegate, route)
}

func compileReviewedKaminoMessage(v *Vault, request KaminoLegRequest, delegate publicKey, route RuntimeRoute) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Kamino blockhash lifetime")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	instructions, _, err := kaminoPolicyInstructions(v, request, delegate, route)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, instructions, executionBudget(route.Lane, len(instructions)))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// kaminoPolicyInstructions is one KLend leg as the chain executes it: the
// route's refresh prefix (two reserve refreshes and the obligation refresh
// over request's topology) followed by the leg's Squads execute at its
// policy constraint. KLend's check_refresh counts back from the top-level
// execute, so the prefix must sit directly in front of it.
func kaminoPolicyInstructions(v *Vault, request KaminoLegRequest, delegate publicKey, route RuntimeRoute) ([]compiledInstruction, kaminoPrimeUSDCLeg, error) {
	inner, leg, err := kaminoResolvedRouteInstruction(request, route)
	if err != nil {
		return nil, 0, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return nil, 0, fmt.Errorf("Kamino request names no installed policy")
	}
	_, index := kaminoPolicyLeg(route, leg)
	outer, err := wrapSquadsKaminoPolicy(v, policy, delegate, delegate, index, inner)
	if err != nil {
		return nil, 0, err
	}
	refresh, err := kaminoRefreshInstructionsForResolvedRoute(request, route)
	if err != nil {
		return nil, 0, err
	}
	return append(refresh, outer), leg, nil
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func kaminoRouteInstruction(v *Vault, request KaminoLegRequest, lane string) (compiledInstruction, kaminoPrimeUSDCLeg, error) {
	route, err := v.route(lane)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	return kaminoResolvedRouteInstruction(request, route)
}

func kaminoResolvedRouteInstruction(request KaminoLegRequest, route RuntimeRoute) (compiledInstruction, kaminoPrimeUSDCLeg, error) {
	lane := request.RouteLane
	if lane == "" {
		lane = route.Lane
	}
	if lane != route.Lane {
		return compiledInstruction{}, 0, fmt.Errorf("Kamino request does not match resolved lane")
	}
	if request.AmountRaw == 0 || len(request.Accounts) == 0 || len(request.Data) != 16 {
		return compiledInstruction{}, 0, fmt.Errorf("incomplete exact Kamino PRIME/USDC packet")
	}
	if got := readU64(request.Data[8:]); got != request.AmountRaw {
		return compiledInstruction{}, 0, fmt.Errorf("Kamino packet amount does not match decision")
	}
	accounts := make([]accountMeta, len(request.Accounts))
	for i, input := range request.Accounts {
		key, err := decodeKey(input.Address)
		if err != nil {
			return compiledInstruction{}, 0, fmt.Errorf("invalid Kamino account %d", i)
		}
		accounts[i] = accountMeta{key: key, signer: input.Signer, writable: input.Writable}
	}
	leg, ok := matchesKaminoStepForResolvedRoute(request.Action, request.Data[:8], accounts, route)
	if !ok {
		return compiledInstruction{}, 0, fmt.Errorf("Kamino packet is not an approved PRIME/USDC lifecycle step")
	}
	return compiledInstruction{program: publicKey(kamino.ProgramID), accounts: accounts, data: append([]byte(nil), request.Data...)}, leg, nil
}

func matchesKaminoStepForResolvedRoute(action Action, discriminator []byte, accounts []accountMeta, route RuntimeRoute) (kaminoPrimeUSDCLeg, bool) {
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	switch action {
	case OpenRouteStep:
		if bytesEqual(discriminator, kamino.DepositV2Discriminator[:]) && exactKaminoMetas(accounts, deposit) {
			return kaminoLegDeposit, true
		}
		if bytesEqual(discriminator, kamino.BorrowV2Discriminator[:]) && exactKaminoMetas(accounts, borrow) {
			return kaminoLegBorrow, true
		}
	case DeleverRouteStep:
		if bytesEqual(discriminator, kamino.RepayV2Discriminator[:]) && exactKaminoMetas(accounts, repay) {
			return kaminoLegRepay, true
		}
		if bytesEqual(discriminator, kamino.WithdrawV2Discriminator[:]) && exactKaminoMetas(accounts, withdraw) {
			return kaminoLegWithdraw, true
		}
	}
	return 0, false
}

func exactKaminoMetas(got, want []accountMeta) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func kaminoPrimeUSDCRefreshInstructionsForRoute(v *Vault, lane string) []compiledInstruction {
	refresh, _ := kaminoPrimeUSDCRefreshInstructionsForRequest(v, KaminoLegRequest{RouteLane: lane})
	return refresh
}

func kaminoPrimeUSDCRefreshInstructionsForRequest(v *Vault, request KaminoLegRequest) ([]compiledInstruction, error) {
	route, err := v.route(request.RouteLane)
	if err != nil {
		return nil, err
	}
	return kaminoRefreshInstructionsForResolvedRoute(request, route)
}

func kaminoRefreshInstructionsForResolvedRoute(request KaminoLegRequest, route RuntimeRoute) ([]compiledInstruction, error) {
	market := kaminoKey(route.Kamino.Market)
	refreshReserve := func(reserve string) compiledInstruction {
		return kaminoCompiled(kamino.RefreshReserve(kamino.RefreshReserveAccounts{Reserve: kaminoKey(reserve), LendingMarket: market, Scope: kaminoKey(kaminoScopePrices)}))
	}
	remaining := request.ObligationReserves
	topology := []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	if len(remaining) > len(topology) || !slices.Equal(remaining, topology[:len(remaining)]) {
		return nil, fmt.Errorf("Kamino obligation topology %v is not the lane's", request.ObligationReserves)
	}
	reserves := make([]solana.PublicKey, len(remaining))
	for i, reserve := range remaining {
		reserves[i] = kaminoKey(reserve)
	}
	return []compiledInstruction{
		refreshReserve(route.Kamino.CollateralReserve), refreshReserve(route.Kamino.DebtReserve),
		kaminoCompiled(kamino.RefreshObligation(market, kaminoKey(route.Kamino.Obligation), reserves...)),
	}, nil
}

func kaminoMetasForRoute(r RuntimeRoute) (deposit, borrow, repay, withdraw []accountMeta) {
	collateral, debt := kaminoRouteAccounts(r, kaminoKey)
	return kaminoCompiled(kamino.DepositV2(collateral, 0)).accounts, kaminoCompiled(kamino.BorrowV2(debt, 0)).accounts,
		kaminoCompiled(kamino.RepayV2(debt, 0)).accounts, kaminoCompiled(kamino.WithdrawV2(collateral, 0)).accounts
}

func kaminoRouteAccounts[T any](r RuntimeRoute, key func(string) T) (kamino.Collateral[T], kamino.Liquidity[T]) {
	vault, obligation, market, authority := key(r.Kamino.Vault), key(r.Kamino.Obligation), key(r.Kamino.Market), key(r.Kamino.MarketAuthority)
	return kamino.Collateral[T]{Owner: vault, Obligation: obligation, LendingMarket: market, LendingMarketAuthority: authority,
			Reserve: key(r.Kamino.CollateralReserve), LiquidityMint: key(r.Kamino.CollateralMint), LiquiditySupply: key(r.CollateralLiquiditySupply),
			CollateralMint: key(r.CollateralReceiptMint), CollateralSupply: key(r.CollateralReceiptSupply), UserLiquidity: key(r.CollateralCustody),
			LiquidityTokenProgram: key(r.CollateralTokenProgram), ObligationFarmUserState: key(r.ObligationCollateralFarm), ReserveFarmState: key(r.CollateralFarm)},
		kamino.Liquidity[T]{Owner: vault, Obligation: obligation, LendingMarket: market, LendingMarketAuthority: authority,
			Reserve: key(r.Kamino.DebtReserve), LiquidityMint: key(r.Kamino.DebtMint), LiquiditySupply: key(r.DebtLiquiditySupply),
			FeeReceiver: key(r.DebtFeeReceiver), UserLiquidity: key(r.DebtCustody), TokenProgram: key(r.DebtTokenProgram),
			ObligationFarmUserState: key(r.ObligationDebtFarm), ReserveFarmState: key(r.DebtFarm)}
}

func kaminoKey(address string) solana.PublicKey {
	if address == "" {
		return solana.PublicKey{}
	}
	return solana.PublicKey(mustKey(address))
}

func kaminoCompiled(ix *solana.GenericInstruction) compiledInstruction {
	out := compiledInstruction{program: publicKey(ix.ProgID), data: ix.DataBytes}
	for _, account := range ix.AccountValues {
		out.accounts = append(out.accounts, accountMeta{key: publicKey(account.PublicKey), signer: account.IsSigner, writable: account.IsWritable})
	}
	return out
}

func wrapSquadsKaminoPolicy(v *Vault, policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != publicKey(kamino.ProgramID) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Kamino policy or delegate")
	}
	return wrapSquadsPolicy(v, policy, executor, []byte{constraintIndex}, inner)
}

type KaminoExecutionEvidence struct {
	Request         KaminoLegRequest
	ExpectedEffects ExpectedEffects
}

func validSHA256(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}
func readU64(value []byte) uint64 {
	var out uint64
	for i := 7; i >= 0; i-- {
		out = out<<8 | uint64(value[i])
	}
	return out
}
