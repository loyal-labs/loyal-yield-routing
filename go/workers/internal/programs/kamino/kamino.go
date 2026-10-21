package kamino

import (
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
)

var (
	ProgramID      = solana.MustPublicKeyFromBase58("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD")
	FarmsProgramID = solana.MustPublicKeyFromBase58("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr")
)

const (
	ReserveSize       = 8624
	ObligationSize    = 3344
	LendingMarketSize = 4664
	UserMetadataSize  = 1032
	FarmUserStateSize = 920
	FarmStateSize     = 8336
)

const (
	VanillaObligationTag  uint8 = 0
	MultiplyObligationTag uint8 = 1
	DefaultObligationID   uint8 = 0
)

var (
	RefreshReserveDiscriminator                = anchor.Instruction("refresh_reserve")
	RefreshObligationDiscriminator             = anchor.Instruction("refresh_obligation")
	DepositV2Discriminator                     = anchor.Instruction("deposit_reserve_liquidity_and_obligation_collateral_v2")
	WithdrawV2Discriminator                    = anchor.Instruction("withdraw_obligation_collateral_and_redeem_reserve_collateral_v2")
	BorrowV2Discriminator                      = anchor.Instruction("borrow_obligation_liquidity_v2")
	RepayV2Discriminator                       = anchor.Instruction("repay_obligation_liquidity_v2")
	InitObligationDiscriminator                = anchor.Instruction("init_obligation")
	InitObligationFarmsForReserveDiscriminator = anchor.Instruction("init_obligation_farms_for_reserve")
	InitUserMetadataDiscriminator              = anchor.Instruction("init_user_metadata")
)

func LendingMarketAuthority(market solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("lma"), market[:]}, ProgramID)
	return key, err
}

func ObligationAddress(tag, id uint8, owner, market, seed1, seed2 solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{{tag}, {id}, owner[:], market[:], seed1[:], seed2[:]}, ProgramID)
	return key, err
}

func VanillaObligation(owner, market solana.PublicKey) (solana.PublicKey, error) {
	return ObligationAddress(VanillaObligationTag, DefaultObligationID, owner, market, solana.PublicKey{}, solana.PublicKey{})
}

func UserMetadataAddress(owner solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("user_meta"), owner[:]}, ProgramID)
	return key, err
}

func ReserveLiquiditySupply(reserve solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("reserve_liq_supply"), reserve[:]}, ProgramID)
	return key, err
}

func ReserveFeeVault(reserve solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("fee_receiver"), reserve[:]}, ProgramID)
	return key, err
}

func ObligationFarmUserState(farm, obligation solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("user"), farm[:], obligation[:]}, FarmsProgramID)
	return key, err
}
