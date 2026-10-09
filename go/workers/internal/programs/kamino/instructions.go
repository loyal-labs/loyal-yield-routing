package kamino

import (
	"encoding/binary"

	"github.com/solana-foundation/solana-go/v2"
)

// Instruction builders for the KLend instructions the workers send, in the
// klend-interface account order. An optional account left as the zero key is
// passed as the KLend program id, read-only (klend-interface optional_account).

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

// CollateralAccounts are the accounts of the v2 deposit and withdrawal, which
// move liquidity between UserLiquidity and the reserve. Collateral mints are
// always classic SPL Token mints.
type CollateralAccounts struct {
	Owner, Obligation, LendingMarket, LendingMarketAuthority, Reserve solana.PublicKey
	LiquidityMint, LiquiditySupply, CollateralMint, CollateralSupply  solana.PublicKey
	UserLiquidity, LiquidityTokenProgram                              solana.PublicKey
	ObligationFarmUserState, ReserveFarmState                         solana.PublicKey // optional
}

// DepositV2 is deposit_reserve_liquidity_and_obligation_collateral_v2.
func DepositV2(a CollateralAccounts, liquidityAmount uint64) *solana.GenericInstruction {
	return instruction(DepositV2Discriminator, amount(liquidityAmount),
		signerWritable(a.Owner), writable(a.Obligation), readonly(a.LendingMarket), readonly(a.LendingMarketAuthority),
		writable(a.Reserve), readonly(a.LiquidityMint), writable(a.LiquiditySupply), writable(a.CollateralMint),
		writable(a.CollateralSupply), writable(a.UserLiquidity), optional(solana.PublicKey{}, false),
		readonly(solana.TokenProgramID), readonly(a.LiquidityTokenProgram), readonly(solana.SysVarInstructionsPubkey),
		optional(a.ObligationFarmUserState, true), optional(a.ReserveFarmState, true), readonly(FarmsProgramID))
}

// WithdrawV2 is withdraw_obligation_collateral_and_redeem_reserve_collateral_v2.
func WithdrawV2(a CollateralAccounts, collateralAmount uint64) *solana.GenericInstruction {
	return instruction(WithdrawV2Discriminator, amount(collateralAmount),
		signerWritable(a.Owner), writable(a.Obligation), readonly(a.LendingMarket), readonly(a.LendingMarketAuthority),
		writable(a.Reserve), readonly(a.LiquidityMint), writable(a.CollateralSupply), writable(a.CollateralMint),
		writable(a.LiquiditySupply), writable(a.UserLiquidity), optional(solana.PublicKey{}, false),
		readonly(solana.TokenProgramID), readonly(a.LiquidityTokenProgram), readonly(solana.SysVarInstructionsPubkey),
		optional(a.ObligationFarmUserState, true), optional(a.ReserveFarmState, true), readonly(FarmsProgramID))
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

// InitObligation creates owner's (tag, id) obligation over seed1 and seed2.
func InitObligation(owner, feePayer, obligation, market, seed1, seed2, ownerUserMetadata solana.PublicKey, tag, id uint8) *solana.GenericInstruction {
	return instruction(InitObligationDiscriminator, []byte{tag, id},
		signer(owner), signerWritable(feePayer), writable(obligation), readonly(market), readonly(seed1), readonly(seed2),
		readonly(ownerUserMetadata), readonly(solana.SysVarRentPubkey), readonly(solana.SystemProgramID))
}

// InitObligationFarmsForReserve creates the obligation's farm user state for
// the reserve farm of mode (0 collateral, 1 debt).
type InitObligationFarmsAccounts struct {
	Payer, Owner, Obligation, LendingMarketAuthority, Reserve solana.PublicKey
	ReserveFarmState, ObligationFarmUserState, LendingMarket  solana.PublicKey
}

func InitObligationFarmsForReserve(a InitObligationFarmsAccounts, mode uint8) *solana.GenericInstruction {
	return instruction(InitObligationFarmsForReserveDiscriminator, []byte{mode},
		signerWritable(a.Payer), readonly(a.Owner), writable(a.Obligation), readonly(a.LendingMarketAuthority),
		writable(a.Reserve), writable(a.ReserveFarmState), writable(a.ObligationFarmUserState), readonly(a.LendingMarket),
		readonly(FarmsProgramID), readonly(solana.SysVarRentPubkey), readonly(solana.SystemProgramID))
}

// InitUserMetadata creates owner's user metadata with no referrer.
func InitUserMetadata(owner, feePayer, userMetadata, userLookupTable solana.PublicKey) *solana.GenericInstruction {
	return instruction(InitUserMetadataDiscriminator, userLookupTable[:],
		signer(owner), signerWritable(feePayer), writable(userMetadata), optional(solana.PublicKey{}, false),
		readonly(solana.SysVarRentPubkey), readonly(solana.SystemProgramID))
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
