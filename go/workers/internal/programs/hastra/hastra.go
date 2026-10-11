package hastra

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

var (
	// StakeProgramID is vault-stake built for the AUTO pool (pool-auto).
	StakeProgramID = solana.MustPublicKeyFromBase58("5uJgCDrQHfA58fPqLsuU14Srg9quxXNHz91cZ54cq4pK")
	// MintProgramID is vault-mint, which mints wYLDS against USDC.
	MintProgramID = solana.MustPublicKeyFromBase58("9WUyNREiPDMgwMh5Gt81Fd3JpiCKxpjZ5Dpq9Bo1RhMV")

	// The three mints, all SPL Token (both programs take Program<Token>):
	// USDC is the mint program's vault mint, wYLDS its mint and the stake
	// program's vault mint, AUTO the stake program's mint.
	USDCMint  = solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	WYLDSMint = solana.MustPublicKeyFromBase58("8fr7WGTVFszfyNWRMXj6fRjZZAnDwmXwEpCrtzmUkdih")
	AUTOMint  = solana.MustPublicKeyFromBase58("GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm")
)

// The stake program's accounts: its PDAs from the source's seeds, and its
// wYLDS pool, which is not a PDA but the token account its
// stake_vault_token_account_config names (checked on mainnet by
// TestHastraTamperOnMainnet).
var (
	StakeConfig             = pda(StakeProgramID, []byte("stake_config"))
	StakeVaultAccountConfig = pda(StakeProgramID, []byte("stake_vault_token_account_config"), StakeConfig[:])
	StakeVaultAuthority     = pda(StakeProgramID, []byte("vault_authority"))
	StakeMintAuthority      = pda(StakeProgramID, []byte("mint_authority"))
	StakePriceConfig        = pda(StakeProgramID, []byte("stake_price_config"), StakeConfig[:])
	StakePool               = solana.MustPublicKeyFromBase58("GtWPVP3KPTJC8z9wPLAop4mPp9jZiRKzL8deDD4PfQ7C")
)

// The mint program's accounts, likewise: its USDC pool is the token account
// its vault_token_account_config names.
var (
	MintConfig             = pda(MintProgramID, []byte("config"))
	MintVaultAccountConfig = pda(MintProgramID, []byte("vault_token_account_config"), MintConfig[:])
	MintMintAuthority      = pda(MintProgramID, []byte("mint_authority"))
	MintPool               = solana.MustPublicKeyFromBase58("CVJvFmYcpqq3u7i5cqDK6dqDkTx14Lg8NwUgsLBswcLu")
)

// Anchor instruction discriminators, sha256("global:<name>")[:8]. Both
// programs name their deposit "deposit".
var (
	DepositDiscriminator = [8]byte{242, 35, 198, 137, 82, 225, 242, 182}
	RedeemDiscriminator  = [8]byte{184, 12, 86, 149, 70, 196, 97, 225}
)

// AmountOffset is where each conversion's amount sits in its data: the u64
// after the discriminator (wYLDS or USDC for a deposit, AUTO for a redeem).
const AmountOffset = 8

// Stake is the signer's side of a stake deposit or redeem: the signer and
// its wYLDS and AUTO token accounts, each of which the program requires the
// signer to own. Every other account is the program's own.
type Stake[T any] struct {
	Signer, UserWYLDS, UserAUTO T
}

type (
	StakeAccounts = Stake[solana.PublicKey]
	StakeAllowed  = Stake[squads.Slot]
)

// Mint is the signer's side of a mint deposit: the signer and its USDC and
// wYLDS token accounts, which the program requires the signer to own.
type Mint[T any] struct {
	Signer, UserUSDC, UserWYLDS T
}

type (
	MintAccounts = Mint[solana.PublicKey]
	MintAllowed  = Mint[squads.Slot]
)

// OwnStake is owner converting between its own wYLDS and AUTO associated
// token accounts.
func OwnStake(owner solana.PublicKey) (StakeAccounts, error) {
	wylds, err := spl.AssociatedTokenAddress(owner, WYLDSMint, solana.TokenProgramID)
	if err != nil {
		return StakeAccounts{}, err
	}
	auto, err := spl.AssociatedTokenAddress(owner, AUTOMint, solana.TokenProgramID)
	return StakeAccounts{Signer: owner, UserWYLDS: wylds, UserAUTO: auto}, err
}

// OwnMint is owner converting its own USDC associated token account into its
// own wYLDS one.
func OwnMint(owner solana.PublicKey) (MintAccounts, error) {
	usdc, err := spl.AssociatedTokenAddress(owner, USDCMint, solana.TokenProgramID)
	if err != nil {
		return MintAccounts{}, err
	}
	wylds, err := spl.AssociatedTokenAddress(owner, WYLDSMint, solana.TokenProgramID)
	return MintAccounts{Signer: owner, UserUSDC: usdc, UserWYLDS: wylds}, err
}

// PinStake is the constraint side of a, every account pinned.
func PinStake(a StakeAccounts) StakeAllowed {
	return StakeAllowed{Signer: squads.Pin(a.Signer), UserWYLDS: squads.Pin(a.UserWYLDS), UserAUTO: squads.Pin(a.UserAUTO)}
}

// PinMint is the constraint side of a, every account pinned.
func PinMint(a MintAccounts) MintAllowed {
	return MintAllowed{Signer: squads.Pin(a.Signer), UserUSDC: squads.Pin(a.UserUSDC), UserWYLDS: squads.Pin(a.UserWYLDS)}
}

// stakeDepositSlots is vault-stake's Deposit order. fixed is what a slot
// holding one of the program's own accounts admits.
func stakeDepositSlots[T any](a Stake[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.ReadOnly(fixed(StakeConfig)), squads.ReadOnly(fixed(StakeVaultAccountConfig)), squads.Writable(fixed(StakePool)),
		squads.ReadOnly(fixed(StakeVaultAuthority)), squads.Writable(fixed(AUTOMint)), squads.Writable(fixed(WYLDSMint)),
		squads.ReadOnly(fixed(StakeMintAuthority)), squads.Signing(a.Signer), squads.Writable(a.UserWYLDS), squads.Writable(a.UserAUTO),
		squads.ReadOnly(fixed(StakePriceConfig)), squads.ReadOnly(fixed(solana.TokenProgramID)),
	}
}

// stakeRedeemSlots is vault-stake's Redeem order. Its legacy unbonding
// ticket is the program id: no ticket (the program creates none any more).
func stakeRedeemSlots[T any](a Stake[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.ReadOnly(fixed(StakeConfig)), squads.ReadOnly(fixed(StakeVaultAccountConfig)), squads.Writable(fixed(StakePool)),
		squads.ReadOnly(fixed(StakeVaultAuthority)), squads.SigningWritable(a.Signer), squads.ReadOnly(fixed(StakeProgramID)),
		squads.Writable(a.UserWYLDS), squads.Writable(a.UserAUTO), squads.Writable(fixed(AUTOMint)), squads.Writable(fixed(WYLDSMint)),
		squads.ReadOnly(fixed(StakePriceConfig)), squads.ReadOnly(fixed(solana.TokenProgramID)),
	}
}

// mintDepositSlots is vault-mint's Deposit order.
func mintDepositSlots[T any](a Mint[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.ReadOnly(fixed(MintConfig)), squads.ReadOnly(fixed(MintVaultAccountConfig)), squads.Writable(fixed(MintPool)),
		squads.Writable(fixed(WYLDSMint)), squads.ReadOnly(fixed(MintMintAuthority)), squads.Signing(a.Signer),
		squads.Writable(a.UserUSDC), squads.Writable(a.UserWYLDS), squads.ReadOnly(fixed(solana.TokenProgramID)),
	}
}

// StakeDeposit stakes amount of the signer's wYLDS into AUTO.
func StakeDeposit(a StakeAccounts, amount uint64) *solana.GenericInstruction {
	return instruction(StakeProgramID, DepositDiscriminator, amount, stakeDepositSlots(a, same))
}

// StakeRedeem burns shares of the signer's AUTO for wYLDS.
func StakeRedeem(a StakeAccounts, shares uint64) *solana.GenericInstruction {
	return instruction(StakeProgramID, RedeemDiscriminator, shares, stakeRedeemSlots(a, same))
}

// MintDeposit mints amount of wYLDS for amount of the signer's USDC.
func MintDeposit(a MintAccounts, amount uint64) *solana.GenericInstruction {
	return instruction(MintProgramID, DepositDiscriminator, amount, mintDepositSlots(a, same))
}

// StakeDepositAllowed admits StakeDeposit over the allowed signer side with
// every program account pinned and any amount: the program fixes the rest
// (its PDAs by seed, its pool by its config, the mints by its config), and
// requires both user accounts to be the signer's, so the conversion moves
// only between the signer's own accounts and the pool.
func StakeDepositAllowed(a StakeAllowed) squads.InstructionConstraintView {
	return allow(StakeProgramID, DepositDiscriminator, stakeDepositSlots(a, squads.Pinned))
}

// StakeRedeemAllowed admits StakeRedeem as StakeDepositAllowed admits
// StakeDeposit, the ticket slot pinned to no ticket.
func StakeRedeemAllowed(a StakeAllowed) squads.InstructionConstraintView {
	return allow(StakeProgramID, RedeemDiscriminator, stakeRedeemSlots(a, squads.Pinned))
}

// MintDepositAllowed admits MintDeposit as StakeDepositAllowed admits
// StakeDeposit.
func MintDepositAllowed(a MintAllowed) squads.InstructionConstraintView {
	return allow(MintProgramID, DepositDiscriminator, mintDepositSlots(a, squads.Pinned))
}

func instruction(program solana.PublicKey, discriminator [8]byte, amount uint64, slots []squads.AccountSlot[solana.PublicKey]) *solana.GenericInstruction {
	return solana.NewInstruction(program, squads.Metas(program, slots), binary.LittleEndian.AppendUint64(discriminator[:], amount))
}

func allow(program solana.PublicKey, discriminator [8]byte, slots []squads.AccountSlot[squads.Slot]) squads.InstructionConstraintView {
	return squads.Allow(program, []squads.DataConstraintView{squads.DataBytes(0, discriminator[:])}, squads.Slots(program, slots))
}

func same(k solana.PublicKey) solana.PublicKey { return k }

func pda(program solana.PublicKey, seeds ...[]byte) solana.PublicKey {
	key, _, err := solana.FindProgramAddress(seeds, program)
	if err != nil {
		panic(err)
	}
	return key
}

// StakePriceConfigDiscriminator is sha256("account:StakePriceConfig")[:8].
var StakePriceConfigDiscriminator = [8]byte{15, 224, 172, 217, 179, 246, 85, 16}

// stakePriceConfigSize is StakePriceConfig::LEN, discriminator included.
const stakePriceConfigSize = 177

// StakePrice is the price every stake conversion runs at: Price wYLDS per
// AUTO, times Scale. Timestamp is the Chainlink observation it was verified
// for; a conversion refuses it once now - Timestamp exceeds MaxStaleness.
type StakePrice struct {
	Price                   *big.Int
	Scale                   uint64
	Timestamp, MaxStaleness int64
}

// DecodeStakePrice decodes the stake program's StakePriceConfig: price i128
// at 136, scale u64 at 152, timestamp i64 at 160, max staleness i64 at 168.
func DecodeStakePrice(a *chain.Account) (StakePrice, error) {
	if a == nil {
		return StakePrice{}, errors.New("Hastra stake price config is absent")
	}
	if a.Owner != StakeProgramID || a.Executable || a.Lamports == 0 || len(a.Data) != stakePriceConfigSize ||
		[8]byte(a.Data[:8]) != StakePriceConfigDiscriminator {
		return StakePrice{}, fmt.Errorf("Hastra stake price config %s envelope or layout drifted", a.Key)
	}
	d := a.Data
	low, high := binary.LittleEndian.Uint64(d[136:]), int64(binary.LittleEndian.Uint64(d[144:]))
	price := new(big.Int).Lsh(big.NewInt(high), 64)
	price.Add(price, new(big.Int).SetUint64(low))
	return StakePrice{Price: price, Scale: binary.LittleEndian.Uint64(d[152:]),
		Timestamp: int64(binary.LittleEndian.Uint64(d[160:])), MaxStaleness: int64(binary.LittleEndian.Uint64(d[168:]))}, nil
}

// Fresh reports whether a conversion at unix time now accepts the price.
func (p StakePrice) Fresh(now int64) bool {
	return p.Timestamp > 0 && now-p.Timestamp <= p.MaxStaleness
}

// AUTOForWYLDS is the AUTO a stake deposit of wylds mints:
// floor(wylds·scale/price). A deposit that mints nothing is refused
// (DepositTooSmall).
func (p StakePrice) AUTOForWYLDS(wylds uint64) (uint64, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	return quote(wylds, new(big.Int).SetUint64(p.Scale), p.Price, "Hastra stake deposit")
}

// WYLDSForAUTO is the wYLDS a stake redeem of auto pays:
// floor(auto·price/scale). A redeem that pays nothing is refused
// (InvalidAmount).
func (p StakePrice) WYLDSForAUTO(auto uint64) (uint64, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	return quote(auto, p.Price, new(big.Int).SetUint64(p.Scale), "Hastra stake redeem")
}

// AUTOToRedeem is the least AUTO whose redeem pays at least wylds:
// ceil(wylds·scale/price), which WYLDSForAUTO maps back to wylds or more.
func (p StakePrice) AUTOToRedeem(wylds uint64) (uint64, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	out := new(big.Int).Mul(new(big.Int).SetUint64(wylds), new(big.Int).SetUint64(p.Scale))
	out.Add(out, p.Price).Sub(out, big.NewInt(1)).Quo(out, p.Price)
	if !out.IsUint64() {
		return 0, errors.New("Hastra redeem size exceeds u64")
	}
	return out.Uint64(), nil
}

func (p StakePrice) valid() error {
	if p.Price == nil || p.Price.Sign() <= 0 || p.Scale == 0 {
		return errors.New("Hastra stake price is not initialized")
	}
	return nil
}

func quote(amount uint64, multiplier, divisor *big.Int, name string) (uint64, error) {
	out := new(big.Int).Mul(new(big.Int).SetUint64(amount), multiplier)
	out.Quo(out, divisor)
	if !out.IsUint64() {
		return 0, fmt.Errorf("%s output exceeds u64", name)
	}
	if out.Sign() == 0 {
		return 0, fmt.Errorf("%s of %d rounds to zero", name, amount)
	}
	return out.Uint64(), nil
}
