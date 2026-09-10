package backyardrwa

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
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
	Action                  Action
	AmountRaw               uint64
	Policy                  string
	PolicyConstraintIndex   byte
	PolicyAccountDataSHA256 string
	Accounts                KaminoPrimeUSDCAccounts
	Data                    []byte
	RecentBlockhash         string
	LastValidBlockHeight    int64
	RouteLane               string
	// FullPayoff requires a fresh finite interest-window bound at build/send.
	// It changes no instruction bytes and never asserts terminal debt by itself.
	FullPayoff         bool   `json:"fullPayoff,omitempty"`
	RepaymentRelease   bool   `json:"repaymentRelease,omitempty"`
	ReleaseDebtIdleRaw uint64 `json:"releaseDebtIdleRaw,omitempty"`
	// ObligationReserves is the exact confirmed deposit-then-borrow reserve
	// sequence currently present in the obligation. RefreshObligation requires
	// this live topology; deriving it from the mutation leg breaks re-deposits.
	ObligationReserves []string
}

type SignedKaminoTransaction struct {
	message              []byte
	signedWire           []byte
	messageSHA256        string
	signedWireSHA256     string
	transactionSignature string
	recentBlockhash      string
	lastValidBlockHeight int64
}

const (
	kaminoPrimeUSDCProgram        = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
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
	kaminoFarmsProgram            = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	mapleDebtFarm                 = "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y"
	mapleObligationDebtFarm       = "CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr"
	kaminoInstructions            = "Sysvar1nstructions1111111111111111111111111"
	solanaPacketBytes             = 1232
)

var (
	// @kamino-finance/klend-sdk 7.3.9 generated v2 discriminators. Keep these
	// paired with the exact account vectors below; the old non-v2 tags have a
	// different farm/account boundary and must never be accepted here.
	kaminoDepositCollateral  = []byte{216, 224, 191, 27, 204, 151, 102, 175}
	kaminoBorrowUSDC         = []byte{161, 128, 143, 245, 171, 199, 194, 6}
	kaminoRepayUSDC          = []byte{116, 174, 213, 76, 180, 53, 210, 144}
	kaminoWithdrawCollateral = []byte{235, 52, 119, 152, 149, 197, 20, 7}
	kaminoRefreshReserve     = []byte{2, 218, 138, 235, 79, 201, 25, 102}
	kaminoRefreshObligation  = []byte{33, 132, 147, 228, 151, 192, 72, 89}
)

type kaminoPrimeUSDCLeg byte

const (
	kaminoLegDeposit kaminoPrimeUSDCLeg = iota + 1
	kaminoLegBorrow
	kaminoLegRepay
	kaminoLegWithdraw
)

// BuildAndSignKaminoPrimeUSDCTransaction creates the legacy Solana wire for
// one exact Kamino SDK instruction under the supplied confirmed Squads policy.
// It signs but never simulates, persists, or broadcasts the result.
func BuildAndSignKaminoPrimeUSDCTransaction(request KaminoPrimeUSDCRequest, executor ed25519.PrivateKey) (SignedKaminoTransaction, error) {
	return buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, executor, mustKey(bridgeDelegate))
}

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
	if request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Kamino blockhash lifetime")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, leg, err := kaminoResolvedRouteInstruction(request, route)
	if err != nil {
		return nil, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) || !validSHA256(request.PolicyAccountDataSHA256) {
		return nil, fmt.Errorf("Kamino policy is not bound to confirmed catalog bytes")
	}
	outer, err := wrapSquadsKaminoPolicy(policy, delegate, delegate, request.PolicyConstraintIndex, inner)
	if err != nil {
		return nil, err
	}
	instructions := append(kaminoRefreshInstructionsForResolvedRoute(leg, request, route), outer)
	message, err := compileKaminoLegacyMessage(delegate, blockhash, instructions)
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func buildAndSignKaminoPrimeUSDCTransactionForDelegate(request KaminoPrimeUSDCRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedKaminoTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.LastValidBlockHeight <= 0 {
		return SignedKaminoTransaction{}, fmt.Errorf("invalid Kamino signing material")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedKaminoTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	message, err := compileKaminoMessageForDelegate(request, expectedDelegate)
	if err != nil {
		return SignedKaminoTransaction{}, err
	}
	signature := ed25519.Sign(executor, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	if len(wire) > solanaPacketBytes {
		return SignedKaminoTransaction{}, fmt.Errorf("Kamino PRIME/USDC packet is %d bytes, exceeds %d", len(wire), solanaPacketBytes)
	}
	messageDigest := sha256.Sum256(message)
	wireDigest := sha256.Sum256(wire)
	return SignedKaminoTransaction{
		message: message, signedWire: wire,
		messageSHA256: hex.EncodeToString(messageDigest[:]), signedWireSHA256: hex.EncodeToString(wireDigest[:]),
		transactionSignature: encodeBase58(signature), recentBlockhash: request.RecentBlockhash,
		lastValidBlockHeight: request.LastValidBlockHeight,
	}, nil
}

// compileKaminoLegacyMessage is intentionally separate from the one-inner-
// instruction bridge compiler. It accepts exactly the reviewed KLend prefix
// (two reserve refreshes and one obligation refresh) followed by one Squads
// policy execution; no general multi-instruction transaction API is exposed.
func compileKaminoLegacyMessage(feePayer, blockhash publicKey, instructions []compiledInstruction) ([]byte, error) {
	if len(instructions) != 4 || instructions[0].program != mustKey(kaminoPrimeUSDCProgram) ||
		instructions[1].program != mustKey(kaminoPrimeUSDCProgram) || instructions[2].program != mustKey(kaminoPrimeUSDCProgram) ||
		instructions[3].program != mustKey(bridgeSquadsProgram) || !bytesEqual(instructions[0].data, kaminoRefreshReserve) ||
		!bytesEqual(instructions[1].data, kaminoRefreshReserve) || !bytesEqual(instructions[2].data, kaminoRefreshObligation) {
		return nil, fmt.Errorf("Kamino transaction is not the exact refresh-plus-policy sequence")
	}
	accounts := []accountMeta{{key: feePayer, signer: true, writable: true}}
	for _, instruction := range instructions {
		for _, account := range instruction.accounts {
			pushOrMergeMeta(&accounts, account)
		}
		pushOrMergeMeta(&accounts, accountMeta{key: instruction.program})
	}
	sort.SliceStable(accounts[1:], func(i, j int) bool { return accountRank(accounts[i+1]) < accountRank(accounts[j+1]) })
	if len(accounts) > math.MaxUint8 {
		return nil, fmt.Errorf("Kamino legacy transaction has too many accounts")
	}
	index := make(map[publicKey]byte, len(accounts))
	var required, readonlySigned, readonlyUnsigned byte
	for i, account := range accounts {
		index[account.key] = byte(i)
		if account.signer {
			required++
			if !account.writable {
				readonlySigned++
			}
		} else if !account.writable {
			readonlyUnsigned++
		}
	}
	message := []byte{required, readonlySigned, readonlyUnsigned}
	message = append(message, encodeShortVec(len(accounts))...)
	for _, account := range accounts {
		message = append(message, account.key[:]...)
	}
	message = append(message, blockhash[:]...)
	message = append(message, encodeShortVec(len(instructions))...)
	for _, instruction := range instructions {
		program, ok := index[instruction.program]
		if !ok || len(instruction.accounts) > math.MaxUint8 {
			return nil, fmt.Errorf("invalid Kamino compiled instruction")
		}
		message = append(message, program, byte(len(instruction.accounts)))
		for _, account := range instruction.accounts {
			accountIndex, ok := index[account.key]
			if !ok {
				return nil, fmt.Errorf("missing Kamino instruction account")
			}
			message = append(message, accountIndex)
		}
		message = append(message, encodeShortVec(len(instruction.data))...)
		message = append(message, instruction.data...)
	}
	return message, nil
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
	if request.PolicyConstraintIndex != kaminoConstraintIndexForRoute(route, leg) {
		return compiledInstruction{}, 0, fmt.Errorf("Kamino packet uses the wrong fixed lane constraint index")
	}
	if route.BasicPolicy {
		family := basicPolicyFamilyForKaminoLeg(leg)
		binding, err := basicPolicyBinding(family)
		if err != nil || request.Policy != binding.Policy {
			return compiledInstruction{}, 0, fmt.Errorf("Kamino policy does not match the basic family binding")
		}
	} else if route.Lane != RouteID {
		binding, ok := route.KaminoPolicies[leg]
		if !ok || request.Policy != binding.Policy || request.PolicyAccountDataSHA256 != binding.DataSHA256 {
			return compiledInstruction{}, 0, fmt.Errorf("Kamino policy does not match the exact route leg binding")
		}
	}
	return compiledInstruction{program: mustKey(kaminoPrimeUSDCProgram), accounts: accounts, data: append([]byte(nil), request.Data...)}, leg, nil
}

func kaminoConstraintIndexForRoute(route RuntimeRoute, leg kaminoPrimeUSDCLeg) byte {
	// Retained split-policy routes still have one constraint at index zero. The
	// four-policy basic set uses the family indexes above; Phase 2 attaches that
	// model to the three runtime lanes.
	if !route.BasicPolicy && (route.Lane == RouteID || len(route.KaminoPolicies) > 0) {
		return 0
	}
	return kaminoConstraintIndex(leg)
}

func kaminoConstraintIndex(leg kaminoPrimeUSDCLeg) byte {
	index := basicPolicyConstraintIndex(leg)
	if index == 0xff {
		return math.MaxUint8
	}
	return index
}

func matchesKaminoStep(action Action, discriminator []byte, accounts []accountMeta) (kaminoPrimeUSDCLeg, bool) {
	return matchesKaminoStepForRoute(action, discriminator, accounts, RouteID)
}

func matchesKaminoStepForRoute(action Action, discriminator []byte, accounts []accountMeta, lane string) (kaminoPrimeUSDCLeg, bool) {
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return 0, false
	}
	return matchesKaminoStepForResolvedRoute(action, discriminator, accounts, route)
}

func matchesKaminoStepForResolvedRoute(action Action, discriminator []byte, accounts []accountMeta, route RuntimeRoute) (kaminoPrimeUSDCLeg, bool) {
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	switch action {
	case OpenRouteStep, OpenPrimeUSDCStep:
		if bytesEqual(discriminator, kaminoDepositCollateral) && exactKaminoMetas(accounts, deposit) {
			return kaminoLegDeposit, true
		}
		if bytesEqual(discriminator, kaminoBorrowUSDC) && exactKaminoMetas(accounts, borrow) {
			return kaminoLegBorrow, true
		}
	case DeleverRouteStep, DeleverPrimeUSDCStep:
		if bytesEqual(discriminator, kaminoRepayUSDC) && exactKaminoMetas(accounts, repay) {
			return kaminoLegRepay, true
		}
		if bytesEqual(discriminator, kaminoWithdrawCollateral) && exactKaminoMetas(accounts, withdraw) {
			return kaminoLegWithdraw, true
		}
	}
	return 0, false
}

func kaminoMeta(address string, signer, writable bool) accountMeta {
	return accountMeta{key: mustKey(address), signer: signer, writable: writable}
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

func kaminoDepositMetas() []accountMeta {
	return []accountMeta{
		kaminoMeta(bridgeVault, true, true), kaminoMeta(kaminoPrimeUSDCObligation, false, true),
		kaminoMeta(kaminoPrimeMarket, false, false), kaminoMeta(kaminoPrimeMarketAuthority, false, false),
		kaminoMeta(kaminoPrimeReserve, false, true), kaminoMeta(kaminoPrimeUSDCCollateralMint, false, false),
		kaminoMeta(kaminoPrimeLiquiditySupply, false, true), kaminoMeta(kaminoPrimeReceiptMint, false, true),
		kaminoMeta(kaminoPrimeReceiptSupply, false, true), kaminoMeta(kaminoPrimeCustody, false, true),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(bridgeTokenProgram, false, false),
		kaminoMeta(bridgeTokenProgram, false, false), kaminoMeta(kaminoInstructions, false, false),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
		kaminoMeta(kaminoFarmsProgram, false, false),
	}
}

func kaminoBorrowMetas() []accountMeta {
	return []accountMeta{
		kaminoMeta(bridgeVault, true, false), kaminoMeta(kaminoPrimeUSDCObligation, false, true),
		kaminoMeta(kaminoPrimeMarket, false, false), kaminoMeta(kaminoPrimeMarketAuthority, false, false),
		kaminoMeta(kaminoUSDCReserve, false, true), kaminoMeta(bridgeUSDC, false, false),
		kaminoMeta(kaminoUSDCLiquiditySupply, false, true), kaminoMeta(kaminoUSDCFeeVault, false, true),
		kaminoMeta(bridgeSquadsATA, false, true), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
		kaminoMeta(bridgeTokenProgram, false, false), kaminoMeta(kaminoInstructions, false, false),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
		kaminoMeta(kaminoFarmsProgram, false, false),
	}
}

func kaminoRepayMetas() []accountMeta {
	return []accountMeta{
		kaminoMeta(bridgeVault, true, false), kaminoMeta(kaminoPrimeUSDCObligation, false, true),
		kaminoMeta(kaminoPrimeMarket, false, false), kaminoMeta(kaminoUSDCReserve, false, true),
		kaminoMeta(bridgeUSDC, false, false), kaminoMeta(kaminoUSDCLiquiditySupply, false, true),
		kaminoMeta(bridgeSquadsATA, false, true), kaminoMeta(bridgeTokenProgram, false, false),
		kaminoMeta(kaminoInstructions, false, false), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoPrimeMarketAuthority, false, false),
		kaminoMeta(kaminoFarmsProgram, false, false),
	}
}

func kaminoWithdrawMetas() []accountMeta {
	return []accountMeta{
		kaminoMeta(bridgeVault, true, true), kaminoMeta(kaminoPrimeUSDCObligation, false, true),
		kaminoMeta(kaminoPrimeMarket, false, false), kaminoMeta(kaminoPrimeMarketAuthority, false, false),
		kaminoMeta(kaminoPrimeReserve, false, true), kaminoMeta(kaminoPrimeUSDCCollateralMint, false, false),
		kaminoMeta(kaminoPrimeReceiptSupply, false, true), kaminoMeta(kaminoPrimeReceiptMint, false, true),
		kaminoMeta(kaminoPrimeLiquiditySupply, false, true), kaminoMeta(kaminoPrimeCustody, false, true),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(bridgeTokenProgram, false, false),
		kaminoMeta(bridgeTokenProgram, false, false), kaminoMeta(kaminoInstructions, false, false),
		kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
		kaminoMeta(kaminoFarmsProgram, false, false),
	}
}

func kaminoPrimeUSDCRefreshInstructions(leg kaminoPrimeUSDCLeg) []compiledInstruction {
	return kaminoPrimeUSDCRefreshInstructionsForRoute(leg, RouteID)
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
	program, market := route.Kamino.Program, route.Kamino.Market
	refreshReserve := func(reserve string) compiledInstruction {
		return compiledInstruction{program: mustKey(program), accounts: []accountMeta{
			kaminoMeta(reserve, false, true), kaminoMeta(market, false, false),
			kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoPrimeUSDCProgram, false, false),
			kaminoMeta(kaminoPrimeUSDCProgram, false, false), kaminoMeta(kaminoScopePrices, false, false),
		}, data: append([]byte(nil), kaminoRefreshReserve...)}
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
	obligationAccounts := []accountMeta{kaminoMeta(market, false, false), kaminoMeta(route.Kamino.Obligation, false, true)}
	for _, reserve := range remaining {
		obligationAccounts = append(obligationAccounts, kaminoMeta(reserve, false, true))
	}
	return []compiledInstruction{
		refreshReserve(route.Kamino.CollateralReserve), refreshReserve(route.Kamino.DebtReserve),
		{program: mustKey(program), accounts: obligationAccounts, data: append([]byte(nil), kaminoRefreshObligation...)},
	}
}

func mapleKaminoMetas() (deposit, borrow, repay, withdraw []accountMeta) {
	return kaminoMetasForRoute(mapleSyrupUSDCUSDC)
}

// The SDK v2 layout is shared; only the bound market/custody/token/farm
// identities vary. Receipt tokens always use classic SPL independently of the
// underlying token program. Absent farm accounts use the Anchor sentinel.
func kaminoMetasForRoute(r RuntimeRoute) (deposit, borrow, repay, withdraw []accountMeta) {
	deposit, borrow, repay, withdraw = kaminoDepositMetas(), kaminoBorrowMetas(), kaminoRepayMetas(), kaminoWithdrawMetas()
	set := func(m []accountMeta, index int, address string) { m[index].key = mustKey(address) }
	farm := func(m []accountMeta, index int, address string) {
		if address == "" {
			m[index] = kaminoMeta(r.Kamino.Program, false, false)
		} else {
			m[index] = kaminoMeta(address, false, true)
		}
	}
	for _, m := range [][]accountMeta{deposit, borrow, repay, withdraw} {
		set(m, 0, r.Kamino.Vault)
		set(m, 1, r.Kamino.Obligation)
		set(m, 2, r.Kamino.Market)
	}
	for _, m := range [][]accountMeta{deposit, withdraw} {
		set(m, 3, r.Kamino.MarketAuthority)
		set(m, 4, r.Kamino.CollateralReserve)
		set(m, 5, r.Kamino.CollateralMint)
		set(m, 9, r.CollateralCustody)
		set(m, 10, r.Kamino.Program)
		set(m, 11, classicTokenProgram)
		set(m, 12, r.CollateralTokenProgram)
		farm(m, 14, r.ObligationCollateralFarm)
		farm(m, 15, r.CollateralFarm)
	}
	set(deposit, 6, r.CollateralLiquiditySupply)
	set(deposit, 7, r.CollateralReceiptMint)
	set(deposit, 8, r.CollateralReceiptSupply)
	set(withdraw, 6, r.CollateralReceiptSupply)
	set(withdraw, 7, r.CollateralReceiptMint)
	set(withdraw, 8, r.CollateralLiquiditySupply)
	set(borrow, 3, r.Kamino.MarketAuthority)
	set(borrow, 4, r.Kamino.DebtReserve)
	set(borrow, 5, r.Kamino.DebtMint)
	set(borrow, 6, r.DebtLiquiditySupply)
	set(borrow, 7, r.DebtFeeReceiver)
	set(borrow, 8, r.DebtCustody)
	set(borrow, 9, r.Kamino.Program)
	set(borrow, 10, r.DebtTokenProgram)
	farm(borrow, 12, r.ObligationDebtFarm)
	farm(borrow, 13, r.DebtFarm)
	set(repay, 3, r.Kamino.DebtReserve)
	set(repay, 4, r.Kamino.DebtMint)
	set(repay, 5, r.DebtLiquiditySupply)
	set(repay, 6, r.DebtCustody)
	set(repay, 7, r.DebtTokenProgram)
	farm(repay, 9, r.ObligationDebtFarm)
	farm(repay, 10, r.DebtFarm)
	set(repay, 11, r.Kamino.MarketAuthority)
	return
}

func wrapSquadsKaminoPolicy(policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != mustKey(kaminoPrimeUSDCProgram) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Kamino policy or delegate")
	}
	transactionAccounts := make([]accountMeta, 0, len(inner.accounts)+1)
	indexes := make([]byte, 0, len(inner.accounts))
	for _, account := range inner.accounts {
		indexes = append(indexes, pushOrMergeMeta(&transactionAccounts, account))
	}
	programIndex := pushOrMergeMeta(&transactionAccounts, accountMeta{key: inner.program})
	for i := range transactionAccounts {
		transactionAccounts[i].signer = false
	}
	compiled := []byte{1, programIndex, byte(len(indexes))}
	compiled = append(compiled, indexes...)
	compiled = appendU16(compiled, uint16(len(inner.data)))
	compiled = append(compiled, inner.data...)
	data := append([]byte(nil), squadsExecuteSyncDiscriminator...)
	data = append(data, 0, 1, 1, 1, 1)
	data = appendU32(data, 1)
	data = append(data, constraintIndex, 1, 0)
	data = appendU32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	accounts := []accountMeta{{key: policy, writable: true}, {key: mustKey(bridgeSquadsProgram)}, {key: executor, signer: true}}
	accounts = append(accounts, transactionAccounts...)
	return compiledInstruction{program: mustKey(bridgeSquadsProgram), accounts: accounts, data: data}, nil
}

func (s SignedKaminoTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 {
		return BuildResult{}, fmt.Errorf("exact signed Kamino transaction was not simulated")
	}
	return BuildResult{MessageSHA256: s.messageSHA256, SignedWire: append([]byte(nil), s.signedWire...), SignedWireSHA256: s.signedWireSHA256, TransactionSignature: s.transactionSignature, RecentBlockhash: s.recentBlockhash, LastValidBlockHeight: s.lastValidBlockHeight, SimulationSlot: simulationSlot}, nil
}

// KaminoExecutionEvidence is intentionally explicit. The observer supplies a
// coherent confirmed account graph and the already-validated expected effects;
// this package only constructs the exact, policy-wrapped, signed wire.
type KaminoExecutionEvidence struct {
	Request         KaminoPrimeUSDCRequest
	ExpectedEffects ExpectedEffects
}

func BuildSimulateAndPersistKamino(ctx context.Context, database *Database, rpc *RPCClient, operationID string, evidence KaminoExecutionEvidence) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("Kamino runtime dependencies are required")
	}
	if _, _, err := kaminoPrimeUSDCInstruction(evidence.Request); err != nil {
		return err
	}
	effects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(effects); err != nil {
		return err
	}
	if err := authorizePhase3ProductionBuild(ctx, database, rpc, operationID, evidence.Request, evidence.ExpectedEffects, effects); err != nil {
		return err
	}
	signer, err := loadPinnedPolicySigner()
	if err != nil {
		return err
	}
	signed, err := BuildAndSignKaminoPrimeUSDCTransaction(evidence.Request, signer)
	if err != nil {
		return err
	}
	if err := database.MarkBuilt(ctx, operationID, signed.messageSHA256, effects); err != nil {
		return err
	}
	simulation, err := rpc.SimulateSignedTransaction(ctx, signed.signedWire)
	if err != nil {
		return err
	}
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
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
