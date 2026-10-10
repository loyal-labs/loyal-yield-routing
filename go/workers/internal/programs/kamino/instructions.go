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
// order written once, as a slot list over the slot type: a key per slot for
// the instruction, a squads.Slot per slot for its policy constraint. The
// builder and its *Allowed constraint read the same list. The lifecycle
// constraints (deposit, withdraw, borrow, repay) leave the slots KLend fills
// itself free: programs, sysvars, the deposit's collateral placeholder and the
// borrow's referrer, which KLend checks against the obligation. The init
// constraints pin every slot, so a delegate cannot set a referrer.

// RefreshReserveAccounts are refresh_reserve's accounts; the oracles are
// optional.
type RefreshReserveAccounts struct {
	Reserve, LendingMarket                         solana.PublicKey
	Pyth, SwitchboardPrice, SwitchboardTWAP, Scope solana.PublicKey
}

func RefreshReserve(a RefreshReserveAccounts) *solana.GenericInstruction {
	return instruction(RefreshReserveDiscriminator, nil,
		writable(a.Reserve), readonly(a.LendingMarket),
		optional(a.Pyth, false), optional(a.SwitchboardPrice, false), optional(a.SwitchboardTWAP, false), optional(a.Scope, false))
}

// RefreshObligation refreshes obligation against reserves, its deposit
// reserves followed by its borrow reserves.
func RefreshObligation(market, obligation solana.PublicKey, reserves ...solana.PublicKey) *solana.GenericInstruction {
	accounts := []*solana.AccountMeta{readonly(market), writable(obligation)}
	for _, reserve := range reserves {
		accounts = append(accounts, writable(reserve))
	}
	return instruction(RefreshObligationDiscriminator, nil, accounts...)
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

func depositV2Slots[T any](a Collateral[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerWritableSlot(a.Owner), writableSlot(a.Obligation), readonlySlot(a.LendingMarket), readonlySlot(a.LendingMarketAuthority),
		writableSlot(a.Reserve), readonlySlot(a.LiquidityMint), writableSlot(a.LiquiditySupply), writableSlot(a.CollateralMint),
		writableSlot(a.CollateralSupply), writableSlot(a.UserLiquidity), readonlySlot(fixed(ProgramID)),
		readonlySlot(fixed(solana.TokenProgramID)), readonlySlot(a.LiquidityTokenProgram), readonlySlot(fixed(solana.SysVarInstructionsPubkey)),
		optionalSlot(a.ObligationFarmUserState, true), optionalSlot(a.ReserveFarmState, true), readonlySlot(fixed(FarmsProgramID)),
	}
}

func withdrawV2Slots[T any](a Collateral[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerWritableSlot(a.Owner), writableSlot(a.Obligation), readonlySlot(a.LendingMarket), readonlySlot(a.LendingMarketAuthority),
		writableSlot(a.Reserve), readonlySlot(a.LiquidityMint), writableSlot(a.CollateralSupply), writableSlot(a.CollateralMint),
		writableSlot(a.LiquiditySupply), writableSlot(a.UserLiquidity), readonlySlot(fixed(ProgramID)),
		readonlySlot(fixed(solana.TokenProgramID)), readonlySlot(a.LiquidityTokenProgram), readonlySlot(fixed(solana.SysVarInstructionsPubkey)),
		optionalSlot(a.ObligationFarmUserState, true), optionalSlot(a.ReserveFarmState, true), readonlySlot(fixed(FarmsProgramID)),
	}
}

// DepositV2 is deposit_reserve_liquidity_and_obligation_collateral_v2.
func DepositV2(a CollateralAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(DepositV2Discriminator, amount(liquidityAmount), metas(depositV2Slots(a, same))...)
}

// DepositV2Allowed admits DepositV2 over the allowed accounts, any amount.
func DepositV2Allowed(a CollateralAllowed) squads.InstructionConstraintView {
	return allow(DepositV2Discriminator[:], depositV2Slots(a, squads.Unpinned))
}

// WithdrawV2 is withdraw_obligation_collateral_and_redeem_reserve_collateral_v2.
func WithdrawV2(a CollateralAccounts, collateralAmount uint64) *solana.GenericInstruction {
	return instruction(WithdrawV2Discriminator, amount(collateralAmount), metas(withdrawV2Slots(a, same))...)
}

// WithdrawV2Allowed admits WithdrawV2 over the allowed accounts, any amount.
func WithdrawV2Allowed(a CollateralAllowed) squads.InstructionConstraintView {
	return allow(WithdrawV2Discriminator[:], withdrawV2Slots(a, squads.Unpinned))
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
func borrowV2Slots[T any](a Liquidity[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), writableSlot(a.Obligation), readonlySlot(a.LendingMarket), readonlySlot(a.LendingMarketAuthority),
		writableSlot(a.Reserve), readonlySlot(a.LiquidityMint), writableSlot(a.LiquiditySupply), writableSlot(a.FeeReceiver),
		writableSlot(a.UserLiquidity), readonlySlot(fixed(ProgramID)), readonlySlot(a.TokenProgram),
		readonlySlot(fixed(solana.SysVarInstructionsPubkey)),
		optionalSlot(a.ObligationFarmUserState, true), optionalSlot(a.ReserveFarmState, true), readonlySlot(fixed(FarmsProgramID)),
	}
}

func repayV2Slots[T any](a Liquidity[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), writableSlot(a.Obligation), readonlySlot(a.LendingMarket), writableSlot(a.Reserve),
		readonlySlot(a.LiquidityMint), writableSlot(a.LiquiditySupply), writableSlot(a.UserLiquidity), readonlySlot(a.TokenProgram),
		readonlySlot(fixed(solana.SysVarInstructionsPubkey)),
		optionalSlot(a.ObligationFarmUserState, true), optionalSlot(a.ReserveFarmState, true),
		readonlySlot(a.LendingMarketAuthority), readonlySlot(fixed(FarmsProgramID)),
	}
}

// BorrowV2 is borrow_obligation_liquidity_v2 with no referrer.
func BorrowV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(BorrowV2Discriminator, amount(liquidityAmount), metas(borrowV2Slots(a, same))...)
}

// BorrowV2Allowed admits BorrowV2 over the allowed accounts, any amount.
func BorrowV2Allowed(a LiquidityAllowed) squads.InstructionConstraintView {
	return allow(BorrowV2Discriminator[:], borrowV2Slots(a, squads.Unpinned))
}

// RepayV2 is repay_obligation_liquidity_v2.
func RepayV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(RepayV2Discriminator, amount(liquidityAmount), metas(repayV2Slots(a, same))...)
}

// RepayV2Allowed admits RepayV2 over the allowed accounts, any amount.
func RepayV2Allowed(a LiquidityAllowed) squads.InstructionConstraintView {
	return allow(RepayV2Discriminator[:], repayV2Slots(a, squads.Unpinned))
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

func initObligationSlots[T any](a ObligationInit[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), signerWritableSlot(a.FeePayer), writableSlot(a.Obligation), readonlySlot(a.LendingMarket),
		readonlySlot(a.Seed1), readonlySlot(a.Seed2), readonlySlot(a.OwnerUserMetadata),
		readonlySlot(fixed(solana.SysVarRentPubkey)), readonlySlot(fixed(solana.SystemProgramID)),
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
	return instruction(InitObligationFarmsForReserveDiscriminator, []byte{mode},
		signerWritable(a.Payer), readonly(a.Owner), writable(a.Obligation), readonly(a.LendingMarketAuthority),
		writable(a.Reserve), writable(a.ReserveFarmState), writable(a.ObligationFarmUserState), readonly(a.LendingMarket),
		readonly(FarmsProgramID), readonly(solana.SysVarRentPubkey), readonly(solana.SystemProgramID))
}

// UserMetadataInit is init_user_metadata's account set; it has no referrer.
type UserMetadataInit[T any] struct {
	Owner, FeePayer, UserMetadata T
}

type (
	UserMetadataInitAccounts = UserMetadataInit[solana.PublicKey]
	UserMetadataInitAllowed  = UserMetadataInit[squads.Slot]
)

func initUserMetadataSlots[T any](a UserMetadataInit[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), signerWritableSlot(a.FeePayer), writableSlot(a.UserMetadata), readonlySlot(fixed(ProgramID)),
		readonlySlot(fixed(solana.SysVarRentPubkey)), readonlySlot(fixed(solana.SystemProgramID)),
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

func readonly(key solana.PublicKey) *solana.AccountMeta { return solana.Meta(key) }
func writable(key solana.PublicKey) *solana.AccountMeta { return solana.Meta(key).WRITE() }
func signer(key solana.PublicKey) *solana.AccountMeta   { return solana.Meta(key).SIGNER() }
func signerWritable(key solana.PublicKey) *solana.AccountMeta {
	return solana.Meta(key).SIGNER().WRITE()
}

func optional(key solana.PublicKey, isWritable bool) *solana.AccountMeta {
	if key.IsZero() {
		return readonly(ProgramID)
	}
	return &solana.AccountMeta{PublicKey: key, IsWritable: isWritable}
}

// slot is one account position of an instruction: a key (or the allowed keys)
// and the position's flags. An optional slot without a key is KLend's program
// id, read-only.
type slot[T any] struct {
	key                        T
	writable, signer, optional bool
}

func readonlySlot[T any](k T) slot[T]       { return slot[T]{key: k} }
func writableSlot[T any](k T) slot[T]       { return slot[T]{key: k, writable: true} }
func signerSlot[T any](k T) slot[T]         { return slot[T]{key: k, signer: true} }
func signerWritableSlot[T any](k T) slot[T] { return slot[T]{key: k, writable: true, signer: true} }
func optionalSlot[T any](k T, isWritable bool) slot[T] {
	return slot[T]{key: k, writable: isWritable, optional: true}
}

func same(k solana.PublicKey) solana.PublicKey { return k }

func metas(slots []slot[solana.PublicKey]) []*solana.AccountMeta {
	out := make([]*solana.AccountMeta, len(slots))
	for i, s := range slots {
		switch {
		case s.optional:
			out[i] = optional(s.key, s.writable)
		default:
			out[i] = &solana.AccountMeta{PublicKey: s.key, IsWritable: s.writable, IsSigner: s.signer}
		}
	}
	return out
}

func allow(data []byte, slots []slot[squads.Slot]) squads.InstructionConstraintView {
	out := make([]squads.Slot, len(slots))
	for i, s := range slots {
		out[i] = s.key
	}
	return squads.Allow(ProgramID, data, out)
}
