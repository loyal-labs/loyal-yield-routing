package backyard

import (
	"encoding/hex"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// KaminoPrimeUSDCAccounts is the exact account list produced by the checked
// Kamino SDK for one leg.  Reserve-owned token accounts are deliberately read
// from the confirmed reserve graph; they are not guessed or derived here.
// The builder accepts no route, mint, or program override.
type KaminoPrimeUSDCAccounts []struct {
	Address  string
	Signer   bool
	Writable bool
}

// KaminoPrimeUSDCRequest describes one policy-wrapped leg of the fixed route.
// OPEN is either its PRIME collateral deposit or USDC borrow leg; DELEVER is
// either its USDC repayment or PRIME collateral withdrawal leg.  A swap is
// intentionally outside this narrow builder and cannot be smuggled in as an
// arbitrary program instruction.
type KaminoPrimeUSDCRequest struct {
	Action    Action
	AmountRaw uint64
	// Policy is the installed account of the leg's policy (kaminoPolicyLeg).
	Policy               string
	Accounts             KaminoPrimeUSDCAccounts
	Data                 []byte
	RecentBlockhash      string
	LastValidBlockHeight int64
	RouteLane            string
	// ObligationReserves is the exact confirmed deposit-then-borrow reserve
	// sequence currently present in the obligation. RefreshObligation requires
	// this live topology; deriving it from the mutation leg breaks re-deposits.
	ObligationReserves []string
}

const (
	kaminoPrimeMarket             = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	kaminoPrimeMarketAuthority    = "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM"
	kaminoPrimeReserve            = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
	kaminoUSDCReserve             = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"
	kaminoPrimeUSDCCollateralMint = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	kaminoPrimeLiquiditySupply    = "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352"
	kaminoPrimeReceiptMint        = "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW"
	kaminoPrimeReceiptSupply      = "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN"
	kaminoUSDCLiquiditySupply     = "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC"
	kaminoUSDCFeeVault            = "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6"
	kaminoPrimeCustody            = "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct"
	kaminoScopePrices             = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"
	mapleDebtFarm                 = "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y"
	mapleObligationDebtFarm       = "CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr"
	kaminoInstructions            = "Sysvar1nstructions1111111111111111111111111"
)

type kaminoPrimeUSDCLeg byte

const (
	kaminoLegDeposit kaminoPrimeUSDCLeg = iota + 1
	kaminoLegBorrow
	kaminoLegRepay
	kaminoLegWithdraw
)

func CompileKaminoMessage(request KaminoPrimeUSDCRequest) ([]byte, error) {
	return compileKaminoMessageForDelegate(request, mustKey(bridgeDelegate))
}

func compileKaminoMessageForDelegate(request KaminoPrimeUSDCRequest, delegate publicKey) ([]byte, error) {
	lane := request.RouteLane
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return nil, err
	}
	return compileResolvedKaminoMessage(request, delegate, route)
}

// The public compiler and signer resolve installed routes before entering this
// shared byte builder. Keeping resolution outside permits offline SDK/SBF parity
// tests without registering candidate routes or changing production authority.
func compileResolvedKaminoMessage(request KaminoPrimeUSDCRequest, delegate publicKey, route RuntimeRoute) ([]byte, error) {
	return compileReviewedKaminoMessage(request, delegate, route)
}

// compileReviewedKaminoMessage is the checked byte builder.
func compileReviewedKaminoMessage(request KaminoPrimeUSDCRequest, delegate publicKey, route RuntimeRoute) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Kamino blockhash lifetime")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	instructions, _, err := kaminoPolicyInstructions(request, delegate, route)
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
func kaminoPolicyInstructions(request KaminoPrimeUSDCRequest, delegate publicKey, route RuntimeRoute) ([]compiledInstruction, kaminoPrimeUSDCLeg, error) {
	inner, leg, err := kaminoResolvedRouteInstruction(request, route)
	if err != nil {
		return nil, 0, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return nil, 0, fmt.Errorf("Kamino request names no installed policy")
	}
	_, index := kaminoPolicyLeg(route, leg)
	outer, err := wrapSquadsKaminoPolicy(policy, delegate, delegate, index, inner)
	if err != nil {
		return nil, 0, err
	}
	return append(kaminoRefreshInstructionsForResolvedRoute(leg, request, route), outer), leg, nil
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

func kaminoPrimeUSDCInstruction(request KaminoPrimeUSDCRequest) (compiledInstruction, kaminoPrimeUSDCLeg, error) {
	return kaminoRouteInstruction(request, request.RouteLane)
}

func kaminoRouteInstruction(request KaminoPrimeUSDCRequest, lane string) (compiledInstruction, kaminoPrimeUSDCLeg, error) {
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	return kaminoResolvedRouteInstruction(request, route)
}

func kaminoResolvedRouteInstruction(request KaminoPrimeUSDCRequest, route RuntimeRoute) (compiledInstruction, kaminoPrimeUSDCLeg, error) {
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
	case OpenRouteStep, OpenPrimeUSDCStep:
		if bytesEqual(discriminator, kamino.DepositV2Discriminator[:]) && exactKaminoMetas(accounts, deposit) {
			return kaminoLegDeposit, true
		}
		if bytesEqual(discriminator, kamino.BorrowV2Discriminator[:]) && exactKaminoMetas(accounts, borrow) {
			return kaminoLegBorrow, true
		}
	case DeleverRouteStep, DeleverPrimeUSDCStep:
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

func kaminoPrimeUSDCRefreshInstructionsForRoute(leg kaminoPrimeUSDCLeg, lane string) []compiledInstruction {
	return kaminoPrimeUSDCRefreshInstructionsForRequest(leg, KaminoPrimeUSDCRequest{RouteLane: lane})
}

func kaminoPrimeUSDCRefreshInstructionsForRequest(leg kaminoPrimeUSDCLeg, request KaminoPrimeUSDCRequest) []compiledInstruction {
	lane := request.RouteLane
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return nil
	}
	return kaminoRefreshInstructionsForResolvedRoute(leg, request, route)
}

func kaminoRefreshInstructionsForResolvedRoute(leg kaminoPrimeUSDCLeg, request KaminoPrimeUSDCRequest, route RuntimeRoute) []compiledInstruction {
	market := kaminoKey(route.Kamino.Market)
	refreshReserve := func(reserve string) compiledInstruction {
		return kaminoCompiled(kamino.RefreshReserve(kamino.RefreshReserveAccounts{Reserve: kaminoKey(reserve), LendingMarket: market, Scope: kaminoKey(kaminoScopePrices)}))
	}
	remaining := request.ObligationReserves
	if remaining == nil {
		switch leg {
		case kaminoLegBorrow:
			remaining = []string{route.Kamino.CollateralReserve}
		case kaminoLegRepay:
			remaining = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		case kaminoLegWithdraw:
			remaining = []string{route.Kamino.CollateralReserve}
		}
	}
	allowed := [][]string{{}, {route.Kamino.CollateralReserve}, {route.Kamino.CollateralReserve, route.Kamino.DebtReserve}}
	valid := false
	for _, candidate := range allowed {
		if len(candidate) != len(remaining) {
			continue
		}
		valid = true
		for i := range candidate {
			if candidate[i] != remaining[i] {
				valid = false
				break
			}
		}
		if valid {
			break
		}
	}
	if !valid {
		return nil
	}
	reserves := make([]solana.PublicKey, len(remaining))
	for i, reserve := range remaining {
		reserves[i] = kaminoKey(reserve)
	}
	return []compiledInstruction{
		refreshReserve(route.Kamino.CollateralReserve), refreshReserve(route.Kamino.DebtReserve),
		kaminoCompiled(kamino.RefreshObligation(market, kaminoKey(route.Kamino.Obligation), reserves...)),
	}
}

// kaminoMetasForRoute is the SDK v2 account layout of a route's four legs;
// only the bound market/custody/token/farm identities vary. An empty farm is
// absent.
func kaminoMetasForRoute(r RuntimeRoute) (deposit, borrow, repay, withdraw []accountMeta) {
	collateral, debt := kaminoRouteAccounts(r, kaminoKey)
	return kaminoCompiled(kamino.DepositV2(collateral, 0)).accounts, kaminoCompiled(kamino.BorrowV2(debt, 0)).accounts,
		kaminoCompiled(kamino.RepayV2(debt, 0)).accounts, kaminoCompiled(kamino.WithdrawV2(collateral, 0)).accounts
}

// kaminoRouteAccounts is the route's collateral and debt account sets, each
// address as key gives it: the keys the route's legs send, or the slots its
// split policies pin. An absent (empty) address is the zero key either way.
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

// kaminoKey is a route address; an empty one is the absent (zero) key.
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

func wrapSquadsKaminoPolicy(policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != publicKey(kamino.ProgramID) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Kamino policy or delegate")
	}
	return wrapSquadsPolicy(policy, executor, []byte{constraintIndex}, inner)
}

// KaminoExecutionEvidence is intentionally explicit. The observer supplies a
// coherent confirmed account graph and the already-validated expected effects;
// this package only constructs the exact, policy-wrapped, signed wire.
type KaminoExecutionEvidence struct {
	Request         KaminoPrimeUSDCRequest
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
