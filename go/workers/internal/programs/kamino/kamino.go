// Package kamino holds the Kamino Lending (KLend) and Kamino Farms facts the
// workers use: program IDs, the account layouts they read, the PDAs they
// derive and the instructions they send. Each fact is defined here once.
// Layouts follow klend-interface 23b9f2b.
package kamino

import (
	"github.com/solana-foundation/solana-go/v2"
)

var (
	ProgramID      = solana.MustPublicKeyFromBase58("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD")
	FarmsProgramID = solana.MustPublicKeyFromBase58("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr")
)

// Account sizes, discriminator included.
const (
	ReserveSize       = 8624
	ObligationSize    = 3344
	LendingMarketSize = 4664
	UserMetadataSize  = 1032
	FarmUserStateSize = 920
)

// Anchor instruction discriminators: sha256("global:<name>")[:8].
var (
	RefreshReserveDiscriminator    = [8]byte{2, 218, 138, 235, 79, 201, 25, 102}
	RefreshObligationDiscriminator = [8]byte{33, 132, 147, 228, 151, 192, 72, 89}
	// deposit_reserve_liquidity_and_obligation_collateral_v2
	DepositV2Discriminator = [8]byte{216, 224, 191, 27, 204, 151, 102, 175}
	// withdraw_obligation_collateral_and_redeem_reserve_collateral_v2
	WithdrawV2Discriminator = [8]byte{235, 52, 119, 152, 149, 197, 20, 7}
	// borrow_obligation_liquidity_v2
	BorrowV2Discriminator = [8]byte{161, 128, 143, 245, 171, 199, 194, 6}
	// repay_obligation_liquidity_v2
	RepayV2Discriminator                       = [8]byte{116, 174, 213, 76, 180, 53, 210, 144}
	InitObligationDiscriminator                = [8]byte{251, 10, 231, 76, 27, 11, 159, 96}
	InitObligationFarmsForReserveDiscriminator = [8]byte{136, 63, 15, 186, 211, 152, 168, 164}
	InitUserMetadataDiscriminator              = [8]byte{117, 169, 176, 69, 197, 23, 15, 162}
)

// LendingMarketAuthority is the market's PDA ["lma", market].
func LendingMarketAuthority(market solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("lma"), market[:]}, ProgramID)
	return key, err
}

// ObligationAddress is the PDA [tag, id, owner, market, seed1, seed2].
func ObligationAddress(tag, id uint8, owner, market, seed1, seed2 solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{{tag}, {id}, owner[:], market[:], seed1[:], seed2[:]}, ProgramID)
	return key, err
}

// VanillaObligation is owner's tag 0, id 0 obligation in market, whose seed
// accounts are the default (zero) key.
func VanillaObligation(owner, market solana.PublicKey) (solana.PublicKey, error) {
	return ObligationAddress(0, 0, owner, market, solana.PublicKey{}, solana.PublicKey{})
}

// UserMetadataAddress is owner's PDA ["user_meta", owner].
func UserMetadataAddress(owner solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("user_meta"), owner[:]}, ProgramID)
	return key, err
}

// ObligationFarmUserState is the Farms PDA ["user", farm, obligation].
func ObligationFarmUserState(farm, obligation solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("user"), farm[:], obligation[:]}, FarmsProgramID)
	return key, err
}
