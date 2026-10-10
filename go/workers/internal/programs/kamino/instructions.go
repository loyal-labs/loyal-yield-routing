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
// the instruction, the allowed keys per slot (nil: any) for its policy
// constraint. The builder and its *Allowed constraint read the same list.

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
	CollateralAllowed  = Collateral[[]solana.PublicKey]
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
	return allow(DepositV2Discriminator[:], depositV2Slots(a, only))
}

// WithdrawV2 is withdraw_obligation_collateral_and_redeem_reserve_collateral_v2.
func WithdrawV2(a CollateralAccounts, collateralAmount uint64) *solana.GenericInstruction {
	return instruction(WithdrawV2Discriminator, amount(collateralAmount), metas(withdrawV2Slots(a, same))...)
}

// WithdrawV2Allowed admits WithdrawV2 over the allowed accounts, any amount.
func WithdrawV2Allowed(a CollateralAllowed) squads.InstructionConstraintView {
	return allow(WithdrawV2Discriminator[:], withdrawV2Slots(a, only))
}

// LiquidityAccounts are the accounts of the v2 borrow and repayment, which
// move liquidity between UserLiquidity and the reserve's supply.
type LiquidityAccounts struct {
	Owner, Obligation, LendingMarket, LendingMarketAuthority, Reserve solana.PublicKey
	LiquidityMint, LiquiditySupply, UserLiquidity, TokenProgram       solana.PublicKey
	FeeReceiver                                                       solana.PublicKey // borrow only
	ObligationFarmUserState, ReserveFarmState                         solana.PublicKey // optional
}

// BorrowV2 is borrow_obligation_liquidity_v2 with no referrer.
func BorrowV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(BorrowV2Discriminator, amount(liquidityAmount),
		signer(a.Owner), writable(a.Obligation), readonly(a.LendingMarket), readonly(a.LendingMarketAuthority),
		writable(a.Reserve), readonly(a.LiquidityMint), writable(a.LiquiditySupply), writable(a.FeeReceiver),
		writable(a.UserLiquidity), optional(solana.PublicKey{}, false), readonly(a.TokenProgram),
		readonly(solana.SysVarInstructionsPubkey),
		optional(a.ObligationFarmUserState, true), optional(a.ReserveFarmState, true), readonly(FarmsProgramID))
}

// RepayV2 is repay_obligation_liquidity_v2.
func RepayV2(a LiquidityAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(RepayV2Discriminator, amount(liquidityAmount),
		signer(a.Owner), writable(a.Obligation), readonly(a.LendingMarket), writable(a.Reserve),
		readonly(a.LiquidityMint), writable(a.LiquiditySupply), writable(a.UserLiquidity), readonly(a.TokenProgram),
		readonly(solana.SysVarInstructionsPubkey),
		optional(a.ObligationFarmUserState, true), optional(a.ReserveFarmState, true),
		readonly(a.LendingMarketAuthority), readonly(FarmsProgramID))
}

// ObligationInit is init_obligation's account set; Seed1 and Seed2 are the
// zero key for a vanilla obligation.
type ObligationInit[T any] struct {
	Owner, FeePayer, Obligation, LendingMarket, Seed1, Seed2, OwnerUserMetadata T
}

func initObligationSlots[T any](a ObligationInit[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), signerWritableSlot(a.FeePayer), writableSlot(a.Obligation), readonlySlot(a.LendingMarket),
		readonlySlot(a.Seed1), readonlySlot(a.Seed2), readonlySlot(a.OwnerUserMetadata),
		readonlySlot(fixed(solana.SysVarRentPubkey)), readonlySlot(fixed(solana.SystemProgramID)),
	}
}

// InitObligation creates owner's (tag, id) obligation over seed1 and seed2.
func InitObligation(owner, feePayer, obligation, market, seed1, seed2, ownerUserMetadata solana.PublicKey, tag, id uint8) *solana.GenericInstruction {
	a := ObligationInit[solana.PublicKey]{owner, feePayer, obligation, market, seed1, seed2, ownerUserMetadata}
	return instruction(InitObligationDiscriminator, []byte{tag, id}, metas(initObligationSlots(a, same))...)
}

// InitObligationAllowed admits InitObligation of the (tag, id) obligation
// over the allowed accounts.
func InitObligationAllowed(a ObligationInit[[]solana.PublicKey], tag, id uint8) squads.InstructionConstraintView {
	return allow(append(InitObligationDiscriminator[:], tag, id), initObligationSlots(a, only))
}

// ObligationFarmsInit is init_obligation_farms_for_reserve's account set.
type ObligationFarmsInit[T any] struct {
	Payer, Owner, Obligation, LendingMarketAuthority, Reserve T
	ReserveFarmState, ObligationFarmUserState, LendingMarket  T
}

type InitObligationFarmsAccounts = ObligationFarmsInit[solana.PublicKey]

func initObligationFarmsSlots[T any](a ObligationFarmsInit[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerWritableSlot(a.Payer), readonlySlot(a.Owner), writableSlot(a.Obligation), readonlySlot(a.LendingMarketAuthority),
		writableSlot(a.Reserve), writableSlot(a.ReserveFarmState), writableSlot(a.ObligationFarmUserState), readonlySlot(a.LendingMarket),
		readonlySlot(fixed(FarmsProgramID)), readonlySlot(fixed(solana.SysVarRentPubkey)), readonlySlot(fixed(solana.SystemProgramID)),
	}
}

// InitObligationFarmsForReserve creates the obligation's farm user state for
// the reserve farm of mode (0 collateral, 1 debt).
func InitObligationFarmsForReserve(a InitObligationFarmsAccounts, mode uint8) *solana.GenericInstruction {
	return instruction(InitObligationFarmsForReserveDiscriminator, []byte{mode}, metas(initObligationFarmsSlots(a, same))...)
}

// InitObligationFarmsForReserveAllowed admits InitObligationFarmsForReserve
// of mode over the allowed accounts.
func InitObligationFarmsForReserveAllowed(a ObligationFarmsInit[[]solana.PublicKey], mode uint8) squads.InstructionConstraintView {
	return allow(append(InitObligationFarmsForReserveDiscriminator[:], mode), initObligationFarmsSlots(a, only))
}

// UserMetadataInit is init_user_metadata's account set; it has no referrer.
type UserMetadataInit[T any] struct {
	Owner, FeePayer, UserMetadata T
}

func initUserMetadataSlots[T any](a UserMetadataInit[T], fixed func(solana.PublicKey) T) []slot[T] {
	return []slot[T]{
		signerSlot(a.Owner), signerWritableSlot(a.FeePayer), writableSlot(a.UserMetadata), readonlySlot(fixed(ProgramID)),
		readonlySlot(fixed(solana.SysVarRentPubkey)), readonlySlot(fixed(solana.SystemProgramID)),
	}
}

// InitUserMetadata creates owner's user metadata with no referrer.
func InitUserMetadata(owner, feePayer, userMetadata, userLookupTable solana.PublicKey) *solana.GenericInstruction {
	a := UserMetadataInit[solana.PublicKey]{owner, feePayer, userMetadata}
	return instruction(InitUserMetadataDiscriminator, userLookupTable[:], metas(initUserMetadataSlots(a, same))...)
}

// InitUserMetadataAllowed admits InitUserMetadata over the allowed accounts
// with the given user lookup table.
func InitUserMetadataAllowed(a UserMetadataInit[[]solana.PublicKey], userLookupTable solana.PublicKey) squads.InstructionConstraintView {
	return allow(append(InitUserMetadataDiscriminator[:], userLookupTable[:]...), initUserMetadataSlots(a, only))
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

func same(k solana.PublicKey) solana.PublicKey   { return k }
func only(k solana.PublicKey) []solana.PublicKey { return []solana.PublicKey{k} }

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

func allow(data []byte, slots []slot[[]solana.PublicKey]) squads.InstructionConstraintView {
	keys := make([][]solana.PublicKey, len(slots))
	for i, s := range slots {
		keys[i] = s.key
	}
	return squads.Allow(ProgramID, data, keys)
}
