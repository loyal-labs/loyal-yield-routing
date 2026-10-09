package autodeposit

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// Official program constants for the two family wires. Every offset below is
// the same constant the fleet executor reads (fleet/route_runtime.go), so the
// wire this family signs and the receipt it verifies share one identity.
const (
	KLendProgramID = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	splTokenID     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	farmsProgramID = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	instructionsID = "Sysvar1nstructions1111111111111111111111111"

	kaminoDepositV2Step           = "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2"
	kaminoDepositDataLength       = 16
	kaminoRefreshReserveDisc      = "02da8aeb4fc91966"
	kaminoRefreshObligationDisc   = "218493e497c04859"
	kaminoDepositV2Disc           = "d8e0bf1bcc9766af"
	depositInstructionAccountSize = 17

	reserveDataLength    = 8624
	reserveDiscriminator = "2bf2ccca1af73b7f"
	obligationDataLength = 3344
)

// The obligation and wrapper discriminators are checked byte-for-byte.
var (
	obligationDiscriminator = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
)

const (
	systemProgramZero             = "11111111111111111111111111111111"
	reserveMarketOffset           = 32
	reserveFarmOffset             = 64
	reserveLiquidityMintOffset    = 128
	reserveLiquiditySupplyOffset  = 160
	reserveLiquidityProgramOffset = 408
	reserveCollateralMintOffset   = 2560
	reserveCollateralSupplyOffset = 2600
	reserveScopePricesOffset      = 5112
	reserveSwitchboardOffset      = 5160
	reserveSwitchboardTWAPOffset  = 5192
	reservePythOffset             = 5224
	obligationMarketOffset        = 32
	obligationOwnerOffset         = 64
	obligationDepositsOffset      = 96
	obligationDepositStride       = 136
	obligationDepositCount        = 8
	obligationBorrowsOffset       = 1208
	obligationBorrowStride        = 200
	obligationBorrowCount         = 5
)

// AccountReader reads one coherent confirmed account set in address order, at
// a slot no older than minSlot (0 for any): the required addresses must all
// exist, an optional one is nil when absent. The production adapter binds it
// to the chain client; tests bind it to fixtures.
type AccountReader func(ctx context.Context, minSlot int64, addresses []string, optional ...string) (int64, []*chain.Account, error)

// ErrRouteNotExecutable reports a destination that cannot take the deposit
// today. No funds have moved when it is returned, and the controller refuses
// the pull instead of preparing custody it could not deposit.
var ErrRouteNotExecutable = errors.New("autodeposit destination is not executable")

// TopUpRoute is the confirmed destination identity one top-up wire is bound
// to. It is read fresh before the pull and again before the top-up wire is
// built, so a signed wire only ever targets the accounts the frozen plan named.
type TopUpRoute struct {
	Position   fleet.KaminoPositionAccounts `json:"position"`
	Obligation string                       `json:"obligation"`
	// MinimumDepositRaw is the smallest liquidity amount that mints one
	// collateral unit at the confirmed reserve's exchange value.
	MinimumDepositRaw uint64 `json:"-"`
}

// SweepWireBuilder is the production WireBuilder: it compiles and signs the
// family's two exact wires — the delegated Subscriptions pull and the
// policy-wrapped KLend top-up — and never produces any other transaction.
// The executor key is injected explicitly; it must be the pinned delegate the
// balance-sweep policy authorizes, and it pays its own fees.
type SweepWireBuilder struct {
	read     AccountReader
	executor ed25519.PrivateKey
	delegate solana.PublicKey
	rent     SetupRentReader
}

// NewSweepWireBuilder validates the signing material and the account reader.
func NewSweepWireBuilder(executor ed25519.PrivateKey, read AccountReader) (*SweepWireBuilder, error) {
	if len(executor) != ed25519.PrivateKeySize {
		return nil, errors.New("autodeposit wire builder requires an ed25519 delegated signer key")
	}
	if !ed25519.NewKeyFromSeed(executor[:ed25519.SeedSize]).Equal(executor) {
		return nil, errors.New("autodeposit signer public half does not match its seed")
	}
	if read == nil {
		return nil, errors.New("autodeposit wire builder requires a confirmed account reader")
	}
	return &SweepWireBuilder{
		read:     read,
		executor: append(ed25519.PrivateKey(nil), executor...),
		delegate: solana.PublicKey(executor.Public().(ed25519.PublicKey)),
	}, nil
}

func (b *SweepWireBuilder) FeePayer() string { return b.delegate.String() }

// wireAmount renders the frozen plan amount as the wire's u64 field.
func wireAmount(plan DepositPlan) (uint64, error) {
	if plan.AmountRaw <= 0 {
		return 0, errors.New("autodeposit wire amount must be positive")
	}
	amount := uint64(plan.AmountRaw)
	if int64(amount) != plan.AmountRaw {
		return 0, errors.New("autodeposit wire amount is outside the u64 range")
	}
	return amount, nil
}

// BuildPull compiles and signs the delegated Subscriptions transfer_recurring
// pull under the target's balance-sweep policy: the recurring delegation moves
// the frozen amount from the wallet USDC ATA into the frozen vault custody ATA.
func (b *SweepWireBuilder) BuildPull(ctx context.Context, request PullWireRequest) (BuiltWire, error) {
	if err := validatePlanPublicKeys(request.Plan, request.RecurringDelegation, request.Plan.Target.SweepPolicyAccount); err != nil {
		return BuiltWire{}, err
	}
	if request.Plan.Target.TokenMint != USDCMint {
		return BuiltWire{}, fmt.Errorf("autodeposit pull is bound to %s, want USDC", request.Plan.Target.TokenMint)
	}
	if request.RecurringDelegation == "" {
		return BuiltWire{}, errors.New("autodeposit pull has no recurring delegation authority")
	}
	blockhash, err := solana.HashFromBase58(request.RecentBlockhash)
	if err != nil {
		return BuiltWire{}, err
	}
	amount, err := wireAmount(request.Plan)
	if err != nil {
		return BuiltWire{}, err
	}
	target := request.Plan.Target
	inner, err := transferRecurring(amount, mustKey(target.Wallet), mustKey(target.VaultPubkey), mustKey(USDCMint), request.RecurringDelegation, target.WalletUsdcAta, target.VaultUsdcAta)
	if err != nil {
		return BuiltWire{}, err
	}
	wrapped, err := b.wrapWithPolicy(ctx, request.Plan, target.SweepPolicyAccount, inner)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.sign(request.Plan, blockhash, request.LastValidBlockHeight, []fleet.RouteInstruction{wrapped})
}

// BuildTopUp compiles and signs the frozen Kamino deposit of the pulled
// custody: the confirmed reserve route is proved against the frozen plan, the
// official KLend proxy builds the refresh-plus-deposit instructions, and the
// deposit instruction is wrapped under the target's policy.
func (b *SweepWireBuilder) BuildTopUp(ctx context.Context, request TopUpWireRequest) (BuiltWire, error) {
	route, err := b.ConfirmTopUpRoute(ctx, request.Plan)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.buildTopUpWithRoute(ctx, request.Plan, route, request.RecentBlockhash, request.LastValidBlockHeight)
}

// buildTopUpWithRoute signs the top-up against an already-confirmed route.
func (b *SweepWireBuilder) buildTopUpWithRoute(ctx context.Context, plan DepositPlan, route TopUpRoute, blockhashValue string, lastValidBlockHeight int64) (BuiltWire, error) {
	blockhash, err := solana.HashFromBase58(blockhashValue)
	if err != nil {
		return BuiltWire{}, err
	}
	instructions, err := b.topUpInstructions(ctx, plan, route)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.sign(plan, blockhash, lastValidBlockHeight, instructions)
}

// topUpInstructions validates the official builder output and actual policy
// permissions without signing. The pre-pull check and top-up use this same path.
func (b *SweepWireBuilder) topUpInstructions(ctx context.Context, plan DepositPlan, route TopUpRoute) ([]fleet.RouteInstruction, error) {
	amount, err := wireAmount(plan)
	if err != nil {
		return nil, err
	}
	built, err := fleet.BuildIdleDeposit(fleet.KaminoIdleDepositRequest{
		Vault: plan.Target.VaultPubkey, Target: route.Position, DepositLiquidityAmount: amount,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: KLend builder refused the frozen deposit: %v", ErrRouteNotExecutable, err)
	}
	instructions := append(append([]fleet.RouteInstruction{}, built.Public...), built.Protected...)
	for i := len(built.Public); i < len(instructions); i++ {
		if instructions[i], err = b.wrapWithPolicy(ctx, plan, plan.Target.RoutePolicyAccount, instructions[i]); err != nil {
			return nil, err
		}
	}
	return instructions, nil
}

// ConfirmTopUpRoute reads and proves the frozen destination against confirmed
// chain state: the reserve must still be the frozen reserve in the frozen
// market with the frozen liquidity mint, the custody must still be the vault's
// USDC ATA, and the obligation must exist, belong to the vault and market, and
// carry no borrows or foreign deposits. The controller calls this BEFORE the
// pull so an unexecutable destination is refused before any wallet funds move.
func (b *SweepWireBuilder) ConfirmTopUpRoute(ctx context.Context, plan DepositPlan) (TopUpRoute, error) {
	if err := validatePlanPublicKeys(plan); err != nil {
		return TopUpRoute{}, err
	}
	if plan.Reserve == "" || plan.Market == "" || plan.LiquidityMint != USDCMint {
		return TopUpRoute{}, fmt.Errorf("%w: frozen plan reserve/market/liquidity identity is incomplete", ErrRouteNotExecutable)
	}
	vault := mustKey(plan.Target.VaultPubkey)
	market := mustKey(plan.Market)
	vaultATA, err := spl.AssociatedTokenAddress(vault, mustKey(USDCMint), solana.TokenProgramID)
	if err != nil {
		return TopUpRoute{}, err
	}
	if vaultATA.String() != plan.Target.VaultUsdcAta {
		return TopUpRoute{}, fmt.Errorf("%w: frozen custody %s is not the vault's USDC ATA", ErrRouteNotExecutable, plan.Target.VaultUsdcAta)
	}
	obligationKey, err := vanillaObligationKey(vault, market)
	if err != nil {
		return TopUpRoute{}, err
	}
	slot, accounts, err := b.read(ctx, 0, []string{plan.Reserve, plan.Market, obligationKey, plan.Target.VaultUsdcAta})
	if err != nil {
		return TopUpRoute{}, err
	}
	if slot <= 0 || len(accounts) != 4 {
		return TopUpRoute{}, errors.New("top-up account snapshot is incomplete")
	}
	reserve, marketAccount, obligation, custody := accounts[0], accounts[1], accounts[2], accounts[3]
	if reserve.Owner.String() != KLendProgramID || len(reserve.Data) != reserveDataLength || reserveDiscriminator != hex.EncodeToString(reserve.Data[:8]) {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s is not a confirmed KLend reserve", ErrRouteNotExecutable, plan.Reserve)
	}
	route := decodeReservePosition(plan.Reserve, reserve.Data)
	if route.Position.Market != plan.Market {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s moved to market %s, want the frozen %s", ErrRouteNotExecutable, plan.Reserve, route.Position.Market, plan.Market)
	}
	if route.Position.LiquidityMint != plan.LiquidityMint {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s liquidity mint is %s, want the frozen %s", ErrRouteNotExecutable, plan.Reserve, route.Position.LiquidityMint, plan.LiquidityMint)
	}
	if route.Position.LiquiditySupply == "" || route.Position.CollateralMint == "" || route.Position.CollateralSupply == "" || route.Position.LiquidityTokenProgram != splTokenID {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s token accounts are not a routable USDC reserve", ErrRouteNotExecutable, plan.Reserve)
	}
	if marketAccount.Owner.String() != KLendProgramID || marketAccount.Executable {
		return TopUpRoute{}, fmt.Errorf("%w: market %s is not a confirmed KLend market", ErrRouteNotExecutable, plan.Market)
	}
	if route.MinimumDepositRaw, err = backyard.KaminoMinimumDepositAmount(backyard.ConfirmedAccount{Address: plan.Reserve, Owner: reserve.Owner.String(), Lamports: reserve.Lamports, Executable: reserve.Executable, Data: reserve.Data}, plan.Market, plan.LiquidityMint); err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s exchange value: %v", ErrRouteNotExecutable, plan.Reserve, err)
	}
	if obligation.Owner.String() != KLendProgramID || len(obligation.Data) != obligationDataLength || hex.EncodeToString(obligation.Data[:8]) != hex.EncodeToString(obligationDiscriminator[:]) {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s does not exist; run the missing-obligation setup before a pull", ErrRouteNotExecutable, obligationKey)
	}
	if key := base58Key(obligation.Data[obligationMarketOffset : obligationMarketOffset+32]); key != plan.Market {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s belongs to market %s, want the frozen %s", ErrRouteNotExecutable, obligationKey, key, plan.Market)
	}
	if key := base58Key(obligation.Data[obligationOwnerOffset : obligationOwnerOffset+32]); key != plan.Target.VaultPubkey {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s belongs to owner %s, want the frozen vault %s", ErrRouteNotExecutable, obligationKey, key, plan.Target.VaultPubkey)
	}
	for i := 0; i < obligationDepositCount; i++ {
		offset := obligationDepositsOffset + i*obligationDepositStride
		if key := base58Key(obligation.Data[offset : offset+32]); key != systemProgramZero {
			route.Position.ObligationDepositReserves = append(route.Position.ObligationDepositReserves, key)
		}
	}
	sort.Strings(route.Position.ObligationDepositReserves)
	for i := 0; i < obligationBorrowCount; i++ {
		offset := obligationBorrowsOffset + i*obligationBorrowStride
		if key := base58Key(obligation.Data[offset : offset+32]); key != systemProgramZero {
			return TopUpRoute{}, fmt.Errorf("%w: obligation %s carries a borrow, so it is not an idle deposit destination", ErrRouteNotExecutable, obligationKey)
		}
	}
	route.Position.ObligationBorrowReserves = []string{}
	// The reserve farm's user state is derived, not stored: it is the Farms
	// PDA ["user", farm, obligation], exactly as the fleet route runtime does.
	if route.Position.ReserveFarmState != "" {
		farmKey, err := solana.PublicKeyFromBase58(route.Position.ReserveFarmState)
		if err != nil {
			return TopUpRoute{}, fmt.Errorf("%w: reserve farm %s is not a public key", ErrRouteNotExecutable, route.Position.ReserveFarmState)
		}
		obligationKey, err := solana.PublicKeyFromBase58(obligationKey)
		if err != nil {
			return TopUpRoute{}, err
		}
		farmsKey, err := findProgramAddress([][]byte{[]byte("user"), farmKey[:], obligationKey[:]}, farmsProgramID)
		if err != nil {
			return TopUpRoute{}, err
		}
		route.Position.ObligationFarmUserState = base58Key(farmsKey[:])
	}
	for _, deposit := range route.Position.ObligationDepositReserves {
		if deposit != plan.Reserve {
			return TopUpRoute{}, fmt.Errorf("%w: obligation %s already deposits reserve %s", ErrRouteNotExecutable, obligationKey, deposit)
		}
	}
	if _, err := usdcTokenAccount(custody, plan.Target.VaultPubkey); err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: %v", ErrRouteNotExecutable, err)
	}
	route.Obligation = obligationKey
	route.Position.Obligation = obligationKey
	route.Position.VaultLiquidityATA = plan.Target.VaultUsdcAta
	instructions, err := b.topUpInstructions(ctx, plan, route)
	if err != nil {
		return TopUpRoute{}, err
	}
	if _, err := b.transaction(solana.Hash{}, instructions); err != nil {
		return TopUpRoute{}, err
	}
	return route, nil
}

// decodeReservePosition reads a confirmed reserve's token-account identity at
// the fleet execution offsets (fleet decodeRouteReserve).
func decodeReservePosition(reserve string, data []byte) TopUpRoute {
	optional := func(offset int) string {
		if key := base58Key(data[offset : offset+32]); key != systemProgramZero {
			return key
		}
		return ""
	}
	market := base58Key(data[reserveMarketOffset : reserveMarketOffset+32])
	route := TopUpRoute{Position: fleet.KaminoPositionAccounts{
		Reserve:                reserve,
		Market:                 market,
		LiquidityMint:          base58Key(data[reserveLiquidityMintOffset : reserveLiquidityMintOffset+32]),
		CollateralMint:         base58Key(data[reserveCollateralMintOffset : reserveCollateralMintOffset+32]),
		LiquiditySupply:        base58Key(data[reserveLiquiditySupplyOffset : reserveLiquiditySupplyOffset+32]),
		CollateralSupply:       base58Key(data[reserveCollateralSupplyOffset : reserveCollateralSupplyOffset+32]),
		LiquidityTokenProgram:  base58Key(data[reserveLiquidityProgramOffset : reserveLiquidityProgramOffset+32]),
		PythOracle:             optional(reservePythOffset),
		SwitchboardPriceOracle: optional(reserveSwitchboardOffset),
		SwitchboardTWAPOracle:  optional(reserveSwitchboardTWAPOffset),
		ScopePrices:            optional(reserveScopePricesOffset),
		ReserveFarmState:       optional(reserveFarmOffset),
	}}
	if key, err := findProgramAddress([][]byte{[]byte("lma"), data[reserveMarketOffset : reserveMarketOffset+32]}, KLendProgramID); err == nil {
		route.Position.MarketAuthority = base58Key(key[:])
	}
	return route
}

// vanillaObligationKey derives the vanilla obligation PDA: seeds
// [tag 0, id 0, vault, market, zero, zero] over the KLend program, exactly as
// derive_kamino_vanilla_obligation does in loyal-actions.
func vanillaObligationKey(vault, market solana.PublicKey) (string, error) {
	var zero [32]byte
	derived, err := findProgramAddress([][]byte{{0}, {0}, vault[:], market[:], zero[:], zero[:]}, KLendProgramID)
	if err != nil {
		return "", err
	}
	return base58Key(derived[:]), nil
}

// usdcTokenAccount is owner's unfrozen SPL Token USDC account, whose balance
// fits the family's int64 range.
func usdcTokenAccount(account *chain.Account, owner string) (spl.TokenAccount, error) {
	held, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return spl.TokenAccount{}, err
	}
	if held.Program != solana.TokenProgramID || held.Frozen || held.Mint.String() != USDCMint || held.Owner.String() != owner || held.Amount > math.MaxInt64 {
		return spl.TokenAccount{}, fmt.Errorf("%s is not %s's USDC token account in the int64 range", account.Key, owner)
	}
	return held, nil
}

// wrapWithPolicy wraps one inner instruction in the Squads
// execute_transaction_sync_v2 envelope the balance-sweep policy authorizes:
// [policy(w), squadsProgram, executor(signer), inner accounts...], with the
// inner signer flags cleared because the executor is the only signer.
func (b *SweepWireBuilder) wrapWithPolicy(ctx context.Context, plan DepositPlan, policyAccount string, inner fleet.RouteInstruction) (fleet.RouteInstruction, error) {
	if policyAccount == "" {
		return fleet.RouteInstruction{}, errors.New("autodeposit wire has no frozen policy account")
	}
	slot, accounts, err := b.read(ctx, 0, []string{policyAccount})
	if err != nil {
		return fleet.RouteInstruction{}, err
	}
	if slot <= 0 || len(accounts) != 1 || accounts[0].Owner != squads.ProgramID || accounts[0].Executable {
		return fleet.RouteInstruction{}, errors.New("policy account evidence is unavailable or has a foreign owner")
	}
	decoded, err := fleet.DecodeSquadsPolicy(accounts[0])
	if err != nil || int(decoded.AccountIndex) != plan.Target.VaultIndex {
		return fleet.RouteInstruction{}, errors.New("policy account index does not match the frozen vault")
	}
	inner.Step = "autodeposit"
	return fleet.BuildPolicyEnvelope(policyAccount, plan.Target.Settings, b.delegate.String(), accounts[0], []fleet.RouteInstruction{inner})
}

// ProveTopUpWire verifies the persisted immutable top-up wire byte-for-byte:
// exactly three instructions — two public KLend refreshes and one
// policy-wrapped deposit — the deposit's exact discriminator, account vector,
// frozen reserve/obligation/custody identity and amount, and the executor as
// the wrapped transaction's only signer. Token balances are never treated as
// Kamino proof; the wire itself is the proof, and the receipt is checked
// against it separately.
func (b *SweepWireBuilder) ProveTopUpWire(plan DepositPlan, attempt DurableAttempt, route TopUpRoute) error {
	tx, err := persistedWireTransaction(attempt)
	if err != nil {
		return err
	}
	message, err := decodeSignedWireMessageTransaction(tx)
	if err != nil {
		return err
	}
	return b.proveTopUpDecoded(plan, attempt, route, message, tx.Message.Signers())
}

// Historical TypeScript attempts can be v0 packets with an independent fee
// payer. Adoption resolves only their pinned lookups and verifies every
// signature; it never rebuilds or resigns the recorded message.
func (b *SweepWireBuilder) ProveTopUpWireContext(ctx context.Context, plan DepositPlan, attempt DurableAttempt, route TopUpRoute) error {
	tx, err := persistedWireTransaction(attempt)
	if err != nil {
		return err
	}
	if err = b.resolvePersistedLookups(ctx, tx); err != nil {
		return err
	}
	message, err := decodeSignedWireMessageTransaction(tx)
	if err != nil {
		return err
	}
	return b.proveTopUpDecoded(plan, attempt, route, message, tx.Message.Signers())
}

func (b *SweepWireBuilder) proveTopUpDecoded(plan DepositPlan, attempt DurableAttempt, route TopUpRoute, message decodedMessage, signers solana.PublicKeySlice) error {
	if err := validatePlanPublicKeys(plan); err != nil {
		return err
	}
	if message.signature != attempt.Signature || message.blockhash != attempt.RecentBlockhash {
		return errors.New("persisted top-up signature or blockhash differs from exact wire")
	}
	if len(message.instructions) != 3 {
		return fmt.Errorf("persisted top-up wire has %d instructions, want refresh, refresh, wrapped deposit", len(message.instructions))
	}
	delegateSigned := false
	for _, signer := range signers {
		delegateSigned = delegateSigned || signer == b.delegate
	}
	if !delegateSigned {
		return errors.New("persisted top-up executor signature is absent")
	}
	kLend := mustKey(KLendProgramID)
	for i, disc := range []string{kaminoRefreshReserveDisc, kaminoRefreshObligationDisc} {
		instruction := message.instructions[i]
		if instruction.program != kLend || len(instruction.data) != 8 || hex.EncodeToString(instruction.data) != disc {
			return fmt.Errorf("persisted top-up wire instruction %d is not the %s refresh", i, disc)
		}
	}
	if len(message.instructions[0].accounts) != 6 || len(message.instructions[1].accounts) < 2 || len(message.instructions[1].accounts) > 3 {
		return errors.New("persisted top-up refresh account layout is invalid")
	}
	optional := func(address string) string {
		if address == "" {
			return KLendProgramID
		}
		return address
	}
	refreshReserve := []string{plan.Reserve, plan.Market, optional(route.Position.PythOracle), optional(route.Position.SwitchboardPriceOracle), optional(route.Position.SwitchboardTWAPOracle), optional(route.Position.ScopePrices)}
	for index, address := range refreshReserve {
		if !keyEqual(message.instructions[0].accounts[index], address) {
			return errors.New("persisted reserve refresh account differs from frozen route")
		}
	}
	if !keyEqual(message.instructions[1].accounts[0], plan.Market) || (len(message.instructions[1].accounts) == 3 && !keyEqual(message.instructions[1].accounts[2], plan.Reserve)) {
		return errors.New("persisted obligation refresh includes a foreign market or deposit")
	}
	// Refresh the reserve first, then the obligation, in frozen identity order.
	if key := message.instructions[0].accounts[0]; !keyEqual(key, plan.Reserve) {
		return fmt.Errorf("persisted top-up wire refreshes reserve %s, want the frozen %s", key, plan.Reserve)
	}
	if key := message.instructions[1].accounts[1]; !keyEqual(key, route.Obligation) {
		return fmt.Errorf("persisted top-up wire refreshes obligation %s, want the derived %s", message.instructions[1].accounts[1], route.Obligation)
	}
	wrapper := squads.Instruction{ProgramID: message.instructions[2].program, Data: message.instructions[2].data}
	for _, key := range message.instructions[2].accounts {
		wrapper.Accounts = append(wrapper.Accounts, solana.AccountMeta{PublicKey: key})
	}
	execute, err := squads.DecodeExecuteTransactionSyncV2(wrapper)
	if err != nil {
		return fmt.Errorf("persisted top-up deposit is not a policy-wrapped execute_transaction_sync_v2: %w", err)
	}
	if execute.Policy != mustKey(plan.Target.RoutePolicyAccount) || execute.Signer != b.delegate {
		return fmt.Errorf("persisted top-up wire wrapper accounts do not match the frozen policy and executor")
	}
	if int(execute.AccountIndex) != plan.Target.VaultIndex || len(execute.Inner) != 1 {
		return errors.New("persisted policy wrapper options differ from the frozen vault")
	}
	inner := decodedInstruction{program: execute.Inner[0].ProgramID, data: execute.Inner[0].Data}
	for _, account := range execute.Inner[0].Accounts {
		inner.accounts = append(inner.accounts, account.PublicKey)
	}
	if inner.program != kLend || len(inner.data) != kaminoDepositDataLength || hex.EncodeToString(inner.data[:8]) != kaminoDepositV2Disc {
		return fmt.Errorf("persisted top-up wrapped instruction is not the KLend deposit v2")
	}
	amount := binary.LittleEndian.Uint64(inner.data[8:16])
	if int64(amount) != plan.AmountRaw || amount > 1<<63-1 {
		return fmt.Errorf("persisted top-up wire deposits %d, want exactly the frozen %d", amount, plan.AmountRaw)
	}
	if len(inner.accounts) != depositInstructionAccountSize {
		return fmt.Errorf("persisted top-up deposit has %d accounts, want %d", len(inner.accounts), depositInstructionAccountSize)
	}
	expected := []string{
		plan.Target.VaultPubkey, route.Obligation, plan.Market, route.Position.MarketAuthority,
		plan.Reserve, plan.LiquidityMint, route.Position.LiquiditySupply,
		route.Position.CollateralMint, route.Position.CollateralSupply,
		plan.Target.VaultUsdcAta, KLendProgramID, splTokenID, route.Position.LiquidityTokenProgram,
		instructionsID,
	}
	for position, address := range expected {
		if address == "" {
			continue
		}
		if !keyEqual(inner.accounts[position], address) {
			return fmt.Errorf("persisted top-up deposit account %d is %s, want the frozen %s", position, inner.accounts[position], address)
		}
	}
	// The optional farm accounts collapse to the KLend program placeholder when
	// the reserve carries no farm.
	optionalFarm := map[int]string{
		14: route.Position.ObligationFarmUserState,
		15: route.Position.ReserveFarmState,
	}
	for position, address := range optionalFarm {
		want := address
		if want == "" {
			want = KLendProgramID
		}
		if !keyEqual(inner.accounts[position], want) {
			return fmt.Errorf("persisted top-up deposit farm account %d is %s, want %s", position, inner.accounts[position], want)
		}
	}
	if !keyEqual(inner.accounts[16], farmsProgramID) {
		return fmt.Errorf("persisted top-up deposit account 16 is %s, want the Farms program", inner.accounts[16])
	}
	return nil
}

// sign compiles, signs and size-bounds one family wire. The digest is the
// sha256 of the exact wire bytes, validated through the shared OwnSignedWire
// contract before anything is persisted.
func (b *SweepWireBuilder) sign(plan DepositPlan, blockhash solana.Hash, lastValidBlockHeight int64, instructions []fleet.RouteInstruction) (BuiltWire, error) {
	if lastValidBlockHeight <= 0 {
		return BuiltWire{}, errors.New("autodeposit wire needs a positive validity window")
	}
	tx, err := b.transaction(blockhash, instructions)
	if err != nil {
		return BuiltWire{}, err
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key != b.delegate {
			return nil
		}
		ownedKey := solana.PrivateKey(b.executor)
		return &ownedKey
	}); err != nil {
		return BuiltWire{}, err
	}
	if len(tx.Signatures) != 1 {
		return BuiltWire{}, errors.New("autodeposit transaction requires an unexpected signer")
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return BuiltWire{}, err
	}
	owned, err := chain.OwnSignedWire(wire, hex.EncodeToString(mustSHA256(wire)))
	if err != nil {
		return BuiltWire{}, err
	}
	return BuiltWire{
		Signature:               tx.Signatures[0].String(),
		SignedTransactionBase64: base64Std.EncodeToString(owned.Bytes()),
		SignedTransactionSHA256: owned.Hash(),
		RecentBlockhash:         base58Key(blockhash[:]),
		LastValidBlockHeight:    lastValidBlockHeight,
	}, nil
}

// transaction checks packet size and signer ownership before any pull can be
// authorized, as well as when the top-up is eventually signed.
func (b *SweepWireBuilder) transaction(blockhash solana.Hash, instructions []fleet.RouteInstruction) (*solana.Transaction, error) {
	sdkInstructions := make([]solana.Instruction, len(instructions))
	for i, ix := range instructions {
		converted, err := sdkInstruction(ix)
		if err != nil {
			return nil, err
		}
		sdkInstructions[i] = converted
	}
	tx, err := solana.NewTransaction(sdkInstructions, blockhash, solana.TransactionPayer(b.delegate))
	if err != nil {
		return nil, err
	}
	if tx.Message.Header.NumRequiredSignatures != 1 {
		return nil, errors.New("autodeposit instruction requires an unexpected signer")
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if 1+64+len(message) > solanaPacketBytes {
		return nil, errors.New("autodeposit signed packet would exceed Solana limit")
	}
	return tx, nil
}

func validatePlanPublicKeys(plan DepositPlan, extra ...string) error {
	keys := []string{plan.Reserve, plan.Market, plan.LiquidityMint, plan.Target.Settings, plan.Target.Wallet, plan.Target.WalletUsdcAta, plan.Target.VaultPubkey, plan.Target.VaultUsdcAta, plan.Target.TokenMint, plan.Target.RoutePolicyAccount}
	for _, key := range append(keys, extra...) {
		if _, err := solana.PublicKeyFromBase58(key); err != nil {
			return errors.New("autodeposit plan contains an invalid public key")
		}
	}
	if plan.Target.VaultIndex < 0 || plan.Target.VaultIndex > 255 {
		return errors.New("autodeposit vault index outside policy ABI")
	}
	vault, _, err := squads.SmartAccountAddress(mustKey(plan.Target.Settings), uint8(plan.Target.VaultIndex))
	if err != nil || vault.String() != plan.Target.VaultPubkey {
		return errors.New("autodeposit vault is not derived from the frozen settings and index")
	}
	return nil
}
