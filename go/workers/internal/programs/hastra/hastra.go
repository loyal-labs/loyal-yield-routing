package hastra

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

var (
	StakeProgramID = solana.MustPublicKeyFromBase58("5uJgCDrQHfA58fPqLsuU14Srg9quxXNHz91cZ54cq4pK")
	MintProgramID  = solana.MustPublicKeyFromBase58("9WUyNREiPDMgwMh5Gt81Fd3JpiCKxpjZ5Dpq9Bo1RhMV")

	usdcMint  = solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	WYLDSMint = solana.MustPublicKeyFromBase58("8fr7WGTVFszfyNWRMXj6fRjZZAnDwmXwEpCrtzmUkdih")
	AUTOMint  = solana.MustPublicKeyFromBase58("GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm")
)

var (
	StakeConfig             = pda(StakeProgramID, []byte("stake_config"))
	StakeVaultAccountConfig = pda(StakeProgramID, []byte("stake_vault_token_account_config"), StakeConfig[:])
	StakeVaultAuthority     = pda(StakeProgramID, []byte("vault_authority"))
	StakeMintAuthority      = pda(StakeProgramID, []byte("mint_authority"))
	StakePriceConfig        = pda(StakeProgramID, []byte("stake_price_config"), StakeConfig[:])
	StakePool               = solana.MustPublicKeyFromBase58("GtWPVP3KPTJC8z9wPLAop4mPp9jZiRKzL8deDD4PfQ7C")
)

var (
	MintConfig             = pda(MintProgramID, []byte("config"))
	MintVaultAccountConfig = pda(MintProgramID, []byte("vault_token_account_config"), MintConfig[:])
	MintMintAuthority      = pda(MintProgramID, []byte("mint_authority"))
	MintPool               = solana.MustPublicKeyFromBase58("CVJvFmYcpqq3u7i5cqDK6dqDkTx14Lg8NwUgsLBswcLu")
)

// Both programs name their deposit "deposit".
var (
	DepositDiscriminator = anchor.Instruction("deposit")
	RedeemDiscriminator  = anchor.Instruction("redeem")
)

const AmountOffset = 8

type Stake[T any] struct {
	Signer, UserWYLDS, UserAUTO T
}

type (
	StakeAccounts = Stake[solana.PublicKey]
	StakeAllowed  = Stake[squads.Slot]
)

type Mint[T any] struct {
	Signer, UserUSDC, UserWYLDS T
}

type (
	MintAccounts = Mint[solana.PublicKey]
	MintAllowed  = Mint[squads.Slot]
)

func OwnStake(owner solana.PublicKey) (StakeAccounts, error) {
	wylds, err := spl.AssociatedTokenAddress(owner, WYLDSMint, solana.TokenProgramID)
	if err != nil {
		return StakeAccounts{}, err
	}
	auto, err := spl.AssociatedTokenAddress(owner, AUTOMint, solana.TokenProgramID)
	return StakeAccounts{Signer: owner, UserWYLDS: wylds, UserAUTO: auto}, err
}

func OwnMint(owner solana.PublicKey) (MintAccounts, error) {
	usdc, err := spl.AssociatedTokenAddress(owner, usdcMint, solana.TokenProgramID)
	if err != nil {
		return MintAccounts{}, err
	}
	wylds, err := spl.AssociatedTokenAddress(owner, WYLDSMint, solana.TokenProgramID)
	return MintAccounts{Signer: owner, UserUSDC: usdc, UserWYLDS: wylds}, err
}

func PinStake(a StakeAccounts) StakeAllowed {
	return StakeAllowed{Signer: squads.Pin(a.Signer), UserWYLDS: squads.Pin(a.UserWYLDS), UserAUTO: squads.Pin(a.UserAUTO)}
}

func PinMint(a MintAccounts) MintAllowed {
	return MintAllowed{Signer: squads.Pin(a.Signer), UserUSDC: squads.Pin(a.UserUSDC), UserWYLDS: squads.Pin(a.UserWYLDS)}
}

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

func mintDepositSlots[T any](a Mint[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.ReadOnly(fixed(MintConfig)), squads.ReadOnly(fixed(MintVaultAccountConfig)), squads.Writable(fixed(MintPool)),
		squads.Writable(fixed(WYLDSMint)), squads.ReadOnly(fixed(MintMintAuthority)), squads.Signing(a.Signer),
		squads.Writable(a.UserUSDC), squads.Writable(a.UserWYLDS), squads.ReadOnly(fixed(solana.TokenProgramID)),
	}
}

func StakeDeposit(a StakeAccounts, amount uint64) *solana.GenericInstruction {
	return instruction(StakeProgramID, DepositDiscriminator, amount, stakeDepositSlots(a, same))
}

func StakeRedeem(a StakeAccounts, shares uint64) *solana.GenericInstruction {
	return instruction(StakeProgramID, RedeemDiscriminator, shares, stakeRedeemSlots(a, same))
}

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

func StakeRedeemAllowed(a StakeAllowed) squads.InstructionConstraintView {
	return allow(StakeProgramID, RedeemDiscriminator, stakeRedeemSlots(a, squads.Pinned))
}

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

var StakePriceConfigDiscriminator = anchor.Account("StakePriceConfig")

const (
	stakePriceConfigSize    = 177
	stakePriceOffset        = 136
	stakeScaleOffset        = 152
	stakeTimestampOffset    = 160
	stakeMaxStalenessOffset = 168
)

type StakePrice struct {
	Price                   *big.Int
	Scale                   uint64
	Timestamp, MaxStaleness int64
}

// DecodeStakePrice decodes the stake program's StakePriceConfig.
func DecodeStakePrice(a *chain.Account) (StakePrice, error) {
	if a == nil {
		return StakePrice{}, errors.New("Hastra stake price config is absent")
	}
	if a.Owner != StakeProgramID || a.Executable || a.Lamports == 0 || len(a.Data) != stakePriceConfigSize ||
		[8]byte(a.Data[:8]) != StakePriceConfigDiscriminator {
		return StakePrice{}, fmt.Errorf("Hastra stake price config %s envelope or layout drifted", a.Key)
	}
	d := a.Data
	low, high := binary.LittleEndian.Uint64(d[stakePriceOffset:]), int64(binary.LittleEndian.Uint64(d[stakePriceOffset+8:]))
	price := new(big.Int).Lsh(big.NewInt(high), 64)
	price.Add(price, new(big.Int).SetUint64(low))
	return StakePrice{Price: price, Scale: binary.LittleEndian.Uint64(d[stakeScaleOffset:]),
		Timestamp: int64(binary.LittleEndian.Uint64(d[stakeTimestampOffset:])), MaxStaleness: int64(binary.LittleEndian.Uint64(d[stakeMaxStalenessOffset:]))}, nil
}

func (p StakePrice) Fresh(now int64) bool {
	return p.Timestamp > 0 && now-p.Timestamp <= p.MaxStaleness
}

func (p StakePrice) AUTOForWYLDS(wylds uint64) (uint64, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	return quote(wylds, new(big.Int).SetUint64(p.Scale), p.Price, "Hastra stake deposit")
}

func (p StakePrice) WYLDSForAUTO(auto uint64) (uint64, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	return quote(auto, p.Price, new(big.Int).SetUint64(p.Scale), "Hastra stake redeem")
}

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
