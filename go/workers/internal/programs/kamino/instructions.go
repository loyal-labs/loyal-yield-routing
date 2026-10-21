package kamino

import (
	"encoding/binary"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

type RefreshReserveAccounts struct {
	Reserve, LendingMarket                         solana.PublicKey
	Pyth, SwitchboardPrice, SwitchboardTWAP, Scope solana.PublicKey
}

func RefreshReserve(a RefreshReserveAccounts) *solana.GenericInstruction {
	return instruction(RefreshReserveDiscriminator, nil, metas([]squads.AccountSlot[solana.PublicKey]{
		squads.Writable(a.Reserve), squads.ReadOnly(a.LendingMarket), squads.Optional(a.Pyth, false),
		squads.Optional(a.SwitchboardPrice, false), squads.Optional(a.SwitchboardTWAP, false), squads.Optional(a.Scope, false)})...)
}

func RefreshObligation(market, obligation solana.PublicKey, reserves ...solana.PublicKey) *solana.GenericInstruction {
	slots := []squads.AccountSlot[solana.PublicKey]{squads.ReadOnly(market), squads.Writable(obligation)}
	for _, reserve := range reserves {
		slots = append(slots, squads.Writable(reserve))
	}
	return instruction(RefreshObligationDiscriminator, nil, metas(slots)...)
}

// Collateral is the account set of the v2 deposit and withdrawal, which move
// liquidity between UserLiquidity and the reserve. Collateral mints are always
// classic SPL Token mints.
type Collateral[T any] struct {
	Owner, Obligation, LendingMarket, LendingMarketAuthority, Reserve T
	LiquidityMint, LiquiditySupply, CollateralMint, CollateralSupply  T
	UserLiquidity, LiquidityTokenProgram                              T
	ObligationFarmUserState, ReserveFarmState                         T
}

type (
	CollateralAccounts = Collateral[solana.PublicKey]
	CollateralAllowed  = Collateral[squads.Slot]
)

func depositV2Slots[T any](a Collateral[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.SigningWritable(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarket), squads.ReadOnly(a.LendingMarketAuthority),
		squads.Writable(a.Reserve), squads.ReadOnly(a.LiquidityMint), squads.Writable(a.LiquiditySupply), squads.Writable(a.CollateralMint),
		squads.Writable(a.CollateralSupply), squads.Writable(a.UserLiquidity), squads.ReadOnly(fixed(ProgramID)),
		squads.ReadOnly(fixed(solana.TokenProgramID)), squads.ReadOnly(a.LiquidityTokenProgram), squads.ReadOnly(fixed(solana.SysVarInstructionsPubkey)),
		squads.Optional(a.ObligationFarmUserState, true), squads.Optional(a.ReserveFarmState, true), squads.ReadOnly(fixed(FarmsProgramID)),
	}
}

func withdrawV2Slots[T any](a Collateral[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.SigningWritable(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarket), squads.ReadOnly(a.LendingMarketAuthority),
		squads.Writable(a.Reserve), squads.ReadOnly(a.LiquidityMint), squads.Writable(a.CollateralSupply), squads.Writable(a.CollateralMint),
		squads.Writable(a.LiquiditySupply), squads.Writable(a.UserLiquidity), squads.ReadOnly(fixed(ProgramID)),
		squads.ReadOnly(fixed(solana.TokenProgramID)), squads.ReadOnly(a.LiquidityTokenProgram), squads.ReadOnly(fixed(solana.SysVarInstructionsPubkey)),
		squads.Optional(a.ObligationFarmUserState, true), squads.Optional(a.ReserveFarmState, true), squads.ReadOnly(fixed(FarmsProgramID)),
	}
}

func DepositV2(a CollateralAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(DepositV2Discriminator, amount(liquidityAmount), metas(depositV2Slots(a, same))...)
}

const AmountOffset = 8

func DepositV2Allowed(a CollateralAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(DepositV2Discriminator[:], depositV2Slots(a, fixed), data...)
}

func WithdrawV2(a CollateralAccounts, collateralAmount uint64) *solana.GenericInstruction {
	return instruction(WithdrawV2Discriminator, amount(collateralAmount), metas(withdrawV2Slots(a, same))...)
}

func WithdrawV2Allowed(a CollateralAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(WithdrawV2Discriminator[:], withdrawV2Slots(a, fixed), data...)
}

type Liquidity[T any] struct {
	Owner, Obligation, LendingMarket, LendingMarketAuthority, Reserve T
	LiquidityMint, LiquiditySupply, UserLiquidity, TokenProgram       T
	FeeReceiver                                                       T
	ObligationFarmUserState, ReserveFarmState                         T
}

type (
	LiquidityAccounts = Liquidity[solana.PublicKey]
	LiquidityAllowed  = Liquidity[squads.Slot]
)

func borrowV2Slots[T any](a Liquidity[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.Signing(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarket), squads.ReadOnly(a.LendingMarketAuthority),
		squads.Writable(a.Reserve), squads.ReadOnly(a.LiquidityMint), squads.Writable(a.LiquiditySupply), squads.Writable(a.FeeReceiver),
		squads.Writable(a.UserLiquidity), squads.ReadOnly(fixed(ProgramID)), squads.ReadOnly(a.TokenProgram),
		squads.ReadOnly(fixed(solana.SysVarInstructionsPubkey)),
		squads.Optional(a.ObligationFarmUserState, true), squads.Optional(a.ReserveFarmState, true), squads.ReadOnly(fixed(FarmsProgramID)),
	}
}

func repayV2Slots[T any](a Liquidity[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.Signing(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarket), squads.Writable(a.Reserve),
		squads.ReadOnly(a.LiquidityMint), squads.Writable(a.LiquiditySupply), squads.Writable(a.UserLiquidity), squads.ReadOnly(a.TokenProgram),
		squads.ReadOnly(fixed(solana.SysVarInstructionsPubkey)),
		squads.Optional(a.ObligationFarmUserState, true), squads.Optional(a.ReserveFarmState, true),
		squads.ReadOnly(a.LendingMarketAuthority), squads.ReadOnly(fixed(FarmsProgramID)),
	}
}

func BorrowV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(BorrowV2Discriminator, amount(liquidityAmount), metas(borrowV2Slots(a, same))...)
}

func BorrowV2Allowed(a LiquidityAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(BorrowV2Discriminator[:], borrowV2Slots(a, fixed), data...)
}

func RepayV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(RepayV2Discriminator, amount(liquidityAmount), metas(repayV2Slots(a, same))...)
}

func RepayV2Allowed(a LiquidityAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(RepayV2Discriminator[:], repayV2Slots(a, fixed), data...)
}

func OwnedObligation(owner solana.PublicKey) squads.Slot {
	program := ProgramID
	return squads.Slot{Owner: &program, Data: []squads.DataConstraintView{{
		DataOffset: obligationOwnerOffset, DataValue: squads.DataValueView{Kind: squads.ValueBytes, Bytes: owner.Bytes()}, Operator: squads.OpEquals,
	}}}
}

// OwnCollateralAllowed is a deposit or withdrawal by owner on any obligation
// it owns, at reserve and from or into custody: KLend checks the rest (the
// market and its authority, the reserve's mints and supplies, the farms and
// the token program all follow from the reserve and obligation). With
// squads.Unpinned it is the Backyard lanes' collateral leg.
func OwnCollateralAllowed(owner solana.PublicKey, reserve, custody squads.Slot) CollateralAllowed {
	free := squads.Any
	return CollateralAllowed{Owner: squads.Pin(owner), Obligation: OwnedObligation(owner), LendingMarket: free,
		LendingMarketAuthority: free, Reserve: reserve, LiquidityMint: free, LiquiditySupply: free, CollateralMint: free,
		CollateralSupply: free, UserLiquidity: custody, LiquidityTokenProgram: free, ObligationFarmUserState: free, ReserveFarmState: free}
}

// OwnDebtAllowed is a borrow or repayment by owner on any obligation it owns,
// in market and into or from custody: KLend checks the rest, the reserve
// among it (a borrow pays only into custody, whatever reserve it draws on).
// With squads.Unpinned it is the Backyard lanes' debt leg.
func OwnDebtAllowed(owner solana.PublicKey, market, custody squads.Slot) LiquidityAllowed {
	free := squads.Any
	return LiquidityAllowed{Owner: squads.Pin(owner), Obligation: OwnedObligation(owner), LendingMarket: market,
		LendingMarketAuthority: free, Reserve: free, LiquidityMint: free, LiquiditySupply: free, UserLiquidity: custody,
		TokenProgram: free, FeeReceiver: free, ObligationFarmUserState: free, ReserveFarmState: free}
}

type ObligationInit[T any] struct {
	Owner, FeePayer, Obligation, LendingMarket, Seed1, Seed2, OwnerUserMetadata T
}

type (
	ObligationInitAccounts = ObligationInit[solana.PublicKey]
	ObligationInitAllowed  = ObligationInit[squads.Slot]
)

func initObligationSlots[T any](a ObligationInit[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.Signing(a.Owner), squads.SigningWritable(a.FeePayer), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarket),
		squads.ReadOnly(a.Seed1), squads.ReadOnly(a.Seed2), squads.ReadOnly(a.OwnerUserMetadata),
		squads.ReadOnly(fixed(solana.SysVarRentPubkey)), squads.ReadOnly(fixed(solana.SystemProgramID)),
	}
}

func InitObligation(a ObligationInitAccounts, tag, id uint8) *solana.GenericInstruction {
	return instruction(InitObligationDiscriminator, []byte{tag, id}, metas(initObligationSlots(a, same))...)
}

func InitObligationAllowed(a ObligationInitAllowed, tag, id uint8) squads.InstructionConstraintView {
	return allow(append(InitObligationDiscriminator[:], tag, id), initObligationSlots(a, squads.Pinned))
}

type ObligationFarmsInitAccounts struct {
	Payer, Owner, Obligation, LendingMarketAuthority, Reserve solana.PublicKey
	ReserveFarmState, ObligationFarmUserState, LendingMarket  solana.PublicKey
}

type FarmMode uint8

const (
	FarmCollateral FarmMode = iota
	FarmDebt
)

// InitObligationFarmsForReserve creates the obligation's farm user state for
// the reserve farm of mode.
func InitObligationFarmsForReserve(a ObligationFarmsInitAccounts, mode FarmMode) *solana.GenericInstruction {
	return instruction(InitObligationFarmsForReserveDiscriminator, []byte{byte(mode)}, metas([]squads.AccountSlot[solana.PublicKey]{
		squads.SigningWritable(a.Payer), squads.ReadOnly(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarketAuthority),
		squads.Writable(a.Reserve), squads.Writable(a.ReserveFarmState), squads.Writable(a.ObligationFarmUserState), squads.ReadOnly(a.LendingMarket),
		squads.ReadOnly(FarmsProgramID), squads.ReadOnly(solana.SysVarRentPubkey), squads.ReadOnly(solana.SystemProgramID)})...)
}

type UserMetadataInit[T any] struct {
	Owner, FeePayer, UserMetadata T
}

type (
	UserMetadataInitAccounts = UserMetadataInit[solana.PublicKey]
	UserMetadataInitAllowed  = UserMetadataInit[squads.Slot]
)

func initUserMetadataSlots[T any](a UserMetadataInit[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.Signing(a.Owner), squads.SigningWritable(a.FeePayer), squads.Writable(a.UserMetadata), squads.ReadOnly(fixed(ProgramID)),
		squads.ReadOnly(fixed(solana.SysVarRentPubkey)), squads.ReadOnly(fixed(solana.SystemProgramID)),
	}
}

func InitUserMetadata(a UserMetadataInitAccounts, userLookupTable solana.PublicKey) *solana.GenericInstruction {
	return instruction(InitUserMetadataDiscriminator, userLookupTable[:], metas(initUserMetadataSlots(a, same))...)
}

func InitUserMetadataAllowed(a UserMetadataInitAllowed, userLookupTable solana.PublicKey) squads.InstructionConstraintView {
	return allow(append(InitUserMetadataDiscriminator[:], userLookupTable[:]...), initUserMetadataSlots(a, squads.Pinned))
}

func instruction(discriminator [8]byte, args []byte, accounts ...*solana.AccountMeta) *solana.GenericInstruction {
	return solana.NewInstruction(ProgramID, accounts, append(discriminator[:], args...))
}

func amount(value uint64) []byte { return binary.LittleEndian.AppendUint64(nil, value) }

func same(k solana.PublicKey) solana.PublicKey { return k }

func metas(slots []squads.AccountSlot[solana.PublicKey]) []*solana.AccountMeta {
	return squads.Metas(ProgramID, slots)
}

func allow(leading []byte, slots []squads.AccountSlot[squads.Slot], data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, append([]squads.DataConstraintView{squads.DataBytes(0, leading)}, data...), squads.Slots(ProgramID, slots))
}
