package backyard

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
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
	// FullPayoff requires a fresh finite interest-window bound at build/send.
	// It changes no instruction bytes and never asserts terminal debt by itself.
	FullPayoff         bool   `json:"fullPayoff,omitempty"`
	RepaymentRelease   bool   `json:"repaymentRelease,omitempty"`
	ReleaseDebtIdleRaw uint64 `json:"releaseDebtIdleRaw,omitempty"`
	// This selects the reviewed pilot release model, never authority. The
	// locked budget must authorize pilot mode at admission, build and send.
	PilotRepaymentRelease bool `json:"pilotRepaymentRelease,omitempty"`
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
	solanaPacketBytes             = 1232
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
	if lane == autoAUTOPYUSD.Lane {
		return compileReviewedKaminoMessage(request, delegate, route)
	}
	return compileResolvedKaminoMessage(request, delegate, route)
}

// The public compiler and signer resolve installed routes before entering this
// shared byte builder. Keeping resolution outside permits offline SDK/SBF parity
// tests without registering candidate routes or changing production authority.
func compileResolvedKaminoMessage(request KaminoPrimeUSDCRequest, delegate publicKey, route RuntimeRoute) ([]byte, error) {
	if request.PilotRepaymentRelease && (!request.RepaymentRelease || request.FullPayoff || !selectorLane(request.RouteLane)) {
		return nil, budgetHold("invalid_pilot_repayment_release")
	}
	return compileReviewedKaminoMessage(request, delegate, route)
}

// compileReviewedKaminoMessage is the checked byte builder behind both
// entries: installed selector lanes pass the public pilot gate above, the AUTO
// lane comes straight here. The pilot release still must be a withdrawal-only
// repayment release in both cases.
func compileReviewedKaminoMessage(request KaminoPrimeUSDCRequest, delegate publicKey, route RuntimeRoute) ([]byte, error) {
	if request.PilotRepaymentRelease && (!request.RepaymentRelease || request.FullPayoff) {
		return nil, budgetHold("invalid_pilot_repayment_release")
	}
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
	if request.PilotRepaymentRelease && (leg != kaminoLegWithdraw || request.Action != DeleverRouteStep) {
		return nil, budgetHold("invalid_pilot_repayment_release")
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return nil, fmt.Errorf("Kamino request names no installed policy")
	}
	_, index := kaminoPolicyLeg(route, leg)
	outer, err := wrapSquadsKaminoPolicy(policy, delegate, delegate, index, inner)
	if err != nil {
		return nil, err
	}
	instructions := append(kaminoRefreshInstructionsForResolvedRoute(leg, request, route), outer)
	// Installed lanes keep the exact installed legacy bytes; the AUTO lane
	// carries the reviewed ComputeBudget heap frame ahead of the identical
	// refresh-plus-policy payload, so the eight-constraint policy parses and
	// executes on the deployed Squads ELF.
	var message []byte
	if route.Lane == autoAUTOPYUSD.Lane {
		message, err = compileAutoKaminoLegacyMessage(delegate, blockhash, instructions)
	} else {
		message, err = compileKaminoLegacyMessage(delegate, blockhash, instructions)
	}
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
	if err := validateKaminoRefreshSequence(instructions); err != nil {
		return nil, err
	}
	return encodeKaminoLegacyMessageBody(feePayer, blockhash, instructions)
}

// validateKaminoRefreshSequence is the exact installed gate: the reviewed KLend
// prefix (two reserve refreshes and one obligation refresh) followed by one
// Squads policy execution, nothing else. The AUTO resource compiler reuses it
// unchanged so its heap-carrying messages validate the identical payload.
func validateKaminoRefreshSequence(instructions []compiledInstruction) error {
	if len(instructions) != 4 || instructions[0].program != publicKey(kamino.ProgramID) ||
		instructions[1].program != publicKey(kamino.ProgramID) || instructions[2].program != publicKey(kamino.ProgramID) ||
		instructions[3].program != publicKey(squads.ProgramID) || !bytesEqual(instructions[0].data, kamino.RefreshReserveDiscriminator[:]) ||
		!bytesEqual(instructions[1].data, kamino.RefreshReserveDiscriminator[:]) || !bytesEqual(instructions[2].data, kamino.RefreshObligationDiscriminator[:]) {
		return fmt.Errorf("Kamino transaction is not the exact refresh-plus-policy sequence")
	}
	return nil
}

// compileAutoKaminoLegacyMessage is the closed AUTO variant: the exact
// refresh-plus-policy sequence validated by the same gate as the installed
// compiler, prefixed by the canonical ComputeBudget heap frame. The five-slot
// body encoder is only reachable through this validated combination — the
// installed compiler keeps its exact-four gate.
func compileAutoKaminoLegacyMessage(feePayer, blockhash publicKey, instructions []compiledInstruction) ([]byte, error) {
	if err := validateKaminoRefreshSequence(instructions); err != nil {
		return nil, err
	}
	heap := autoComputeBudgetHeapInstruction()
	if !isAutoExecutionHeapInstruction(heap) {
		return nil, fmt.Errorf("AUTO heap resource drifted from the reviewed frame")
	}
	return encodeKaminoLegacyMessageBody(feePayer, blockhash, withAutoExecutionHeap(instructions))
}

// encodeKaminoLegacyMessageBody serializes an already-validated instruction
// list into the legacy wire format. Count and sequence admission stay with the
// callers above; this body never adds or drops instructions.
func encodeKaminoLegacyMessageBody(feePayer, blockhash publicKey, instructions []compiledInstruction) ([]byte, error) {
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

func BuildSimulateAndPersistKamino(ctx context.Context, database *Database, rpc *chain.Client, view *View, operationID string, evidence KaminoExecutionEvidence, credentials Credentials) error {
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
	if err := database.requireBoundIntent(ctx, operationID, evidence.Request, effects); err != nil {
		return err
	}
	if err := validateBuildPrestate(ctx, rpc, view, evidence.Request, evidence.ExpectedEffects); err != nil {
		return err
	}
	signer, err := credentials.signer()
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
	simulation, err := simulateSigned(ctx, rpc, signed.signedWire)
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
