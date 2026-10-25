package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const (
	instructionsID = "Sysvar1nstructions1111111111111111111111111"

	kaminoDepositDataLength       = 16
	depositInstructionAccountSize = 17
)

const (
	systemProgramZero = "11111111111111111111111111111111"
)

type AccountReader func(ctx context.Context, minSlot int64, addresses []string, optional ...string) (int64, []*chain.Account, error)

var ErrRouteNotExecutable = errors.New("autodeposit destination is not executable")

type TopUpRoute struct {
	Position          fleet.KaminoPositionAccounts `json:"position"`
	Obligation        string                       `json:"obligation"`
	MinimumDepositRaw uint64                       `json:"-"`
	DepositRoomRaw    uint64                       `json:"-"`
}

type SweepWireBuilder struct {
	read     AccountReader
	executor ed25519.PrivateKey
	delegate solana.PublicKey
	rent     SetupRentReader
}

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

func wireAmount(amountRaw int64) (uint64, error) {
	if amountRaw <= 0 {
		return 0, errors.New("autodeposit wire amount must be positive")
	}
	amount := uint64(amountRaw)
	if int64(amount) != amountRaw {
		return 0, errors.New("autodeposit wire amount is outside the u64 range")
	}
	return amount, nil
}

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
	amount, err := wireAmount(request.Plan.AmountRaw)
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

func (b *SweepWireBuilder) BuildTopUp(ctx context.Context, request TopUpWireRequest) (BuiltWire, error) {
	route, err := b.ConfirmTopUpRoute(ctx, request.Plan)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.buildTopUpWithRoute(ctx, request.Plan, request.Plan.AmountRaw+request.ResidueRaw, route, request.RecentBlockhash, request.LastValidBlockHeight)
}

func (b *SweepWireBuilder) buildTopUpWithRoute(ctx context.Context, plan DepositPlan, amountRaw int64, route TopUpRoute, blockhashValue string, lastValidBlockHeight int64) (BuiltWire, error) {
	blockhash, err := solana.HashFromBase58(blockhashValue)
	if err != nil {
		return BuiltWire{}, err
	}
	instructions, err := b.topUpInstructions(ctx, plan, amountRaw, route)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.sign(plan, blockhash, lastValidBlockHeight, instructions)
}

func (b *SweepWireBuilder) topUpInstructions(ctx context.Context, plan DepositPlan, amountRaw int64, route TopUpRoute) ([]fleet.RouteInstruction, error) {
	amount, err := wireAmount(amountRaw)
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
	obligationKey, err := kamino.VanillaObligation(vault, market)
	if err != nil {
		return TopUpRoute{}, err
	}
	slot, accounts, err := b.read(ctx, 0, []string{plan.Reserve, plan.Market, obligationKey.String(), plan.Target.VaultUsdcAta})
	if err != nil {
		return TopUpRoute{}, err
	}
	if slot <= 0 || len(accounts) != 4 {
		return TopUpRoute{}, errors.New("top-up account snapshot is incomplete")
	}
	reserveAccount, marketAccount, obligationAccount, custody := accounts[0], accounts[1], accounts[2], accounts[3]
	reserve, err := kamino.DecodeReserve(reserveAccount)
	if err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s is not a confirmed KLend reserve", ErrRouteNotExecutable, plan.Reserve)
	}
	route, err := reserveRoute(reserveAccount, plan.Target.VaultPubkey)
	if err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s is not a confirmed KLend reserve", ErrRouteNotExecutable, plan.Reserve)
	}
	if route.Position.Market != plan.Market {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s moved to market %s, want the frozen %s", ErrRouteNotExecutable, plan.Reserve, route.Position.Market, plan.Market)
	}
	if route.Position.LiquidityMint != plan.LiquidityMint {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s liquidity mint is %s, want the frozen %s", ErrRouteNotExecutable, plan.Reserve, route.Position.LiquidityMint, plan.LiquidityMint)
	}
	if route.Position.LiquidityTokenProgram != earncatalog.SPLTokenProgram {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s token accounts are not a routable USDC reserve", ErrRouteNotExecutable, plan.Reserve)
	}
	if marketAccount.Owner != kamino.ProgramID || marketAccount.Executable {
		return TopUpRoute{}, fmt.Errorf("%w: market %s is not a confirmed KLend market", ErrRouteNotExecutable, plan.Market)
	}
	if route.MinimumDepositRaw, err = reserve.MinimumDeposit(); err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s exchange value: %v", ErrRouteNotExecutable, plan.Reserve, err)
	}
	if route.DepositRoomRaw, err = depositRoom(reserve); err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: reserve %s total supply: %v", ErrRouteNotExecutable, plan.Reserve, err)
	}
	obligation, err := kamino.DecodeObligation(obligationAccount)
	if err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s does not exist; run the missing-obligation setup before a pull", ErrRouteNotExecutable, obligationKey)
	}
	if obligation.LendingMarket != market {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s belongs to market %s, want the frozen %s", ErrRouteNotExecutable, obligationKey, obligation.LendingMarket, plan.Market)
	}
	if obligation.Owner != vault {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s belongs to owner %s, want the frozen vault %s", ErrRouteNotExecutable, obligationKey, obligation.Owner, plan.Target.VaultPubkey)
	}
	if len(obligation.BorrowReserves()) != 0 {
		return TopUpRoute{}, fmt.Errorf("%w: obligation %s carries a borrow, so it is not an idle deposit destination", ErrRouteNotExecutable, obligationKey)
	}
	route.Position.ObligationBorrowReserves = []string{}
	for _, deposit := range obligation.DepositReserves() {
		if deposit.String() != plan.Reserve {
			return TopUpRoute{}, fmt.Errorf("%w: obligation %s already deposits reserve %s", ErrRouteNotExecutable, obligationKey, deposit)
		}
		route.Position.ObligationDepositReserves = append(route.Position.ObligationDepositReserves, deposit.String())
	}
	if _, err := usdcTokenAccount(custody, plan.Target.VaultPubkey); err != nil {
		return TopUpRoute{}, fmt.Errorf("%w: %v", ErrRouteNotExecutable, err)
	}
	instructions, err := b.topUpInstructions(ctx, plan, plan.AmountRaw, route)
	if err != nil {
		return TopUpRoute{}, err
	}
	if _, err := b.transaction(solana.Hash{}, instructions); err != nil {
		return TopUpRoute{}, err
	}
	return route, nil
}

func depositRoom(reserve kamino.Reserve) (uint64, error) {
	total, err := reserve.TotalLiquiditySF()
	if err != nil {
		return 0, err
	}
	supplied := new(big.Int).Add(total, big.NewInt(1<<kamino.FractionBits-1))
	supplied.Rsh(supplied, kamino.FractionBits)
	if !supplied.IsUint64() || supplied.Uint64() >= reserve.DepositLimit {
		return 0, nil
	}
	return reserve.DepositLimit - supplied.Uint64(), nil
}

func reserveRoute(reserve *chain.Account, vault string) (TopUpRoute, error) {
	position, err := fleet.DecodeReserveIdentity(reserve)
	if err == nil {
		position, err = fleet.DeriveVaultReserveAccounts(position, vault)
	}
	return TopUpRoute{Position: position, Obligation: position.Obligation}, err
}

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

func (b *SweepWireBuilder) ProveTopUpWire(ctx context.Context, plan DepositPlan, attempt DurableAttempt, route TopUpRoute, residueBoundRaw int64) error {
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
	return b.proveTopUpDecoded(plan, attempt, route, residueBoundRaw, message, tx.Message.Signers())
}

func (b *SweepWireBuilder) proveTopUpDecoded(plan DepositPlan, attempt DurableAttempt, route TopUpRoute, residueBoundRaw int64, message decodedMessage, signers solana.PublicKeySlice) error {
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
	kLend := kamino.ProgramID
	for i, disc := range [][8]byte{kamino.RefreshReserveDiscriminator, kamino.RefreshObligationDiscriminator} {
		instruction := message.instructions[i]
		if instruction.program != kLend || !bytes.Equal(instruction.data, disc[:]) {
			return fmt.Errorf("persisted top-up wire instruction %d is not the %x refresh", i, disc)
		}
	}
	if len(message.instructions[0].accounts) != 6 || len(message.instructions[1].accounts) < 2 || len(message.instructions[1].accounts) > 3 {
		return errors.New("persisted top-up refresh account layout is invalid")
	}
	optional := func(address string) string {
		if address == "" {
			return kamino.ProgramID.String()
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
	if inner.program != kLend || len(inner.data) != kaminoDepositDataLength || !bytes.Equal(inner.data[:8], kamino.DepositV2Discriminator[:]) {
		return fmt.Errorf("persisted top-up wrapped instruction is not the KLend deposit v2")
	}
	amount := binary.LittleEndian.Uint64(inner.data[8:16])
	if amount > 1<<63-1 || int64(amount) != attempt.AmountRaw || int64(amount) < plan.AmountRaw || residueBoundRaw < 0 || int64(amount)-plan.AmountRaw > residueBoundRaw {
		return fmt.Errorf("persisted top-up wire deposits %d, want exactly the persisted %d, between the frozen %d and %d more", amount, attempt.AmountRaw, plan.AmountRaw, residueBoundRaw)
	}
	if len(inner.accounts) != depositInstructionAccountSize {
		return fmt.Errorf("persisted top-up deposit has %d accounts, want %d", len(inner.accounts), depositInstructionAccountSize)
	}
	expected := []string{
		plan.Target.VaultPubkey, route.Obligation, plan.Market, route.Position.MarketAuthority,
		plan.Reserve, plan.LiquidityMint, route.Position.LiquiditySupply,
		route.Position.CollateralMint, route.Position.CollateralSupply,
		plan.Target.VaultUsdcAta, kamino.ProgramID.String(), earncatalog.SPLTokenProgram, route.Position.LiquidityTokenProgram,
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
			want = kamino.ProgramID.String()
		}
		if !keyEqual(inner.accounts[position], want) {
			return fmt.Errorf("persisted top-up deposit farm account %d is %s, want %s", position, inner.accounts[position], want)
		}
	}
	if !keyEqual(inner.accounts[16], kamino.FarmsProgramID.String()) {
		return fmt.Errorf("persisted top-up deposit account 16 is %s, want the Farms program", inner.accounts[16])
	}
	return nil
}

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
