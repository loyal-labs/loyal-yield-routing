package kamino

import (
	"encoding/binary"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// Instruction builders for the KLend instructions the workers send, in the
// klend-interface account order. An optional account left as the zero key is
// passed as the KLend program id, read-only (klend-interface optional_account).
//
// An instruction a smart account sends through a Squads policy has its account
// order written once, as a squads.AccountSlot list over the key type: a key
// per slot for the instruction, a squads.Slot per slot for its policy
// constraint. The
// builder and its *Allowed constraint read the same list. The lifecycle
// constraints (deposit, withdraw, borrow, repay) take fixed, which says what
// the policy admits in the slots KLend fills itself (see DepositV2Allowed). The init
// constraints pin every slot, so a delegate cannot set a referrer.

// RefreshReserveAccounts are refresh_reserve's accounts; the oracles are
// optional.
type RefreshReserveAccounts struct {
	Reserve, LendingMarket                         solana.PublicKey
	Pyth, SwitchboardPrice, SwitchboardTWAP, Scope solana.PublicKey
}

func RefreshReserve(a RefreshReserveAccounts) *solana.GenericInstruction {
	return instruction(RefreshReserveDiscriminator, nil, metas([]squads.AccountSlot[solana.PublicKey]{
		squads.Writable(a.Reserve), squads.ReadOnly(a.LendingMarket), squads.Optional(a.Pyth, false),
		squads.Optional(a.SwitchboardPrice, false), squads.Optional(a.SwitchboardTWAP, false), squads.Optional(a.Scope, false)})...)
}

// RefreshObligation refreshes obligation against reserves, its deposit
// reserves followed by its borrow reserves.
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
	ObligationFarmUserState, ReserveFarmState                         T // optional
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

// DepositV2 is deposit_reserve_liquidity_and_obligation_collateral_v2.
func DepositV2(a CollateralAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(DepositV2Discriminator, amount(liquidityAmount), metas(depositV2Slots(a, same))...)
}

// AmountOffset is where the amount of a v2 deposit, withdrawal, borrow or
// repayment sits in its data: the u64 after the discriminator.
const AmountOffset = 8

// DepositV2Allowed admits DepositV2 over the allowed accounts with any amount
// the data predicates after its discriminator admit (a bound at AmountOffset,
// say). fixed says what the policy admits in exactly the slots KLend fills
// itself: the token and farms programs, the instructions sysvar, and KLend's
// program id standing in for the collateral placeholder (deposit and
// withdrawal) or the referrer (borrow). squads.Unpinned leaves them to KLend's
// own checks; squads.Pinned pins them. Every other slot is the caller's.
func DepositV2Allowed(a CollateralAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(DepositV2Discriminator[:], depositV2Slots(a, fixed), data...)
}

// WithdrawV2 is withdraw_obligation_collateral_and_redeem_reserve_collateral_v2.
func WithdrawV2(a CollateralAccounts, collateralAmount uint64) *solana.GenericInstruction {
	return instruction(WithdrawV2Discriminator, amount(collateralAmount), metas(withdrawV2Slots(a, same))...)
}

// WithdrawV2Allowed admits WithdrawV2 as DepositV2Allowed admits DepositV2.
func WithdrawV2Allowed(a CollateralAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(WithdrawV2Discriminator[:], withdrawV2Slots(a, fixed), data...)
}

// Liquidity is the account set of the v2 borrow and repayment, which move
// liquidity between UserLiquidity and the reserve's supply.
type Liquidity[T any] struct {
	Owner, Obligation, LendingMarket, LendingMarketAuthority, Reserve T
	LiquidityMint, LiquiditySupply, UserLiquidity, TokenProgram       T
	FeeReceiver                                                       T // borrow only
	ObligationFarmUserState, ReserveFarmState                         T // optional
}

type (
	LiquidityAccounts = Liquidity[solana.PublicKey]
	LiquidityAllowed  = Liquidity[squads.Slot]
)

// borrowV2Slots has no referrer: its optional slot is KLend's program id.
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

// BorrowV2 is borrow_obligation_liquidity_v2 with no referrer.
func BorrowV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(BorrowV2Discriminator, amount(liquidityAmount), metas(borrowV2Slots(a, same))...)
}

// BorrowV2Allowed admits BorrowV2 as DepositV2Allowed admits DepositV2.
func BorrowV2Allowed(a LiquidityAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(BorrowV2Discriminator[:], borrowV2Slots(a, fixed), data...)
}

// RepayV2 is repay_obligation_liquidity_v2.
func RepayV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(RepayV2Discriminator, amount(liquidityAmount), metas(repayV2Slots(a, same))...)
}

// RepayV2Allowed admits RepayV2 as DepositV2Allowed admits DepositV2.
func RepayV2Allowed(a LiquidityAllowed, fixed func(solana.PublicKey) squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(RepayV2Discriminator[:], repayV2Slots(a, fixed), data...)
}

// OwnedObligation is the slot that admits any KLend obligation owned by owner,
// whatever its market, tag or id: the obligation's owner field equals owner.
func OwnedObligation(owner solana.PublicKey) squads.Slot {
	program := ProgramID
	return squads.Slot{Owner: &program, Data: []squads.DataConstraintView{{
		DataOffset: obligationOwnerOffset, DataValue: squads.DataValueView{Kind: 5, Bytes: owner.Bytes()}, Operator: squads.OpEquals,
	}}}
}

// ObligationInit is init_obligation's account set; Seed1 and Seed2 are the
// zero key for a vanilla obligation.
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

// InitObligation creates owner's (tag, id) obligation over seed1 and seed2.
func InitObligation(a ObligationInitAccounts, tag, id uint8) *solana.GenericInstruction {
	return instruction(InitObligationDiscriminator, []byte{tag, id}, metas(initObligationSlots(a, same))...)
}

// InitObligationAllowed admits InitObligation of the (tag, id) obligation
// over the allowed accounts.
func InitObligationAllowed(a ObligationInitAllowed, tag, id uint8) squads.InstructionConstraintView {
	return allow(append(InitObligationDiscriminator[:], tag, id), initObligationSlots(a, squads.Pinned))
}

// ObligationFarmsInitAccounts is init_obligation_farms_for_reserve's account
// set.
type ObligationFarmsInitAccounts struct {
	Payer, Owner, Obligation, LendingMarketAuthority, Reserve solana.PublicKey
	ReserveFarmState, ObligationFarmUserState, LendingMarket  solana.PublicKey
}

// InitObligationFarmsForReserve creates the obligation's farm user state for
// the reserve farm of mode (0 collateral, 1 debt).
func InitObligationFarmsForReserve(a ObligationFarmsInitAccounts, mode uint8) *solana.GenericInstruction {
	return instruction(InitObligationFarmsForReserveDiscriminator, []byte{mode}, metas([]squads.AccountSlot[solana.PublicKey]{
		squads.SigningWritable(a.Payer), squads.ReadOnly(a.Owner), squads.Writable(a.Obligation), squads.ReadOnly(a.LendingMarketAuthority),
		squads.Writable(a.Reserve), squads.Writable(a.ReserveFarmState), squads.Writable(a.ObligationFarmUserState), squads.ReadOnly(a.LendingMarket),
		squads.ReadOnly(FarmsProgramID), squads.ReadOnly(solana.SysVarRentPubkey), squads.ReadOnly(solana.SystemProgramID)})...)
}

// UserMetadataInit is init_user_metadata's account set; it has no referrer.
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

// InitUserMetadata creates owner's user metadata with no referrer.
func InitUserMetadata(a UserMetadataInitAccounts, userLookupTable solana.PublicKey) *solana.GenericInstruction {
	return instruction(InitUserMetadataDiscriminator, userLookupTable[:], metas(initUserMetadataSlots(a, same))...)
}

// InitUserMetadataAllowed admits InitUserMetadata over the allowed accounts
// with the given user lookup table.
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

// allow is the constraint over slots with the data predicates after the
// leading data.
func allow(leading []byte, slots []squads.AccountSlot[squads.Slot], data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, append([]squads.DataConstraintView{squads.DataBytes(0, leading)}, data...), squads.Slots(ProgramID, slots))
}
