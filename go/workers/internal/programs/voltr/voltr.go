// Package voltr holds the Voltr vault program facts the workers use, each
// defined once: the program ID, the account layouts we read (Vault,
// StrategyInitReceipt and RequestWithdrawVaultReceipt), the withdrawal
// receipt PDA and the deposit_strategy and withdraw_strategy instructions we
// send.
package voltr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// ProgramID is the Voltr vault program.
var ProgramID = solana.MustPublicKeyFromBase58("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8")

// Anchor discriminators: sha256("account:<Name>")[:8] and
// sha256("global:<instruction>")[:8].
var (
	VaultDiscriminator             = [8]byte{211, 8, 232, 43, 2, 152, 117, 119}
	StrategyReceiptDiscriminator   = [8]byte{51, 8, 192, 253, 115, 78, 112, 214}
	WithdrawalReceiptDiscriminator = [8]byte{203, 81, 223, 141, 175, 108, 101, 114}
	DepositStrategyDiscriminator   = [8]byte{246, 82, 57, 226, 131, 222, 253, 249}
	WithdrawStrategyDiscriminator  = [8]byte{31, 45, 162, 5, 193, 217, 134, 188}
)

// Deployed account sizes.
const (
	VaultSize             = 928
	StrategyReceiptSize   = 192
	WithdrawalReceiptSize = 112
)

// Vault is the part of a Vault account the workers read.
type Vault struct {
	AssetMint, IdleATA, LPMint, Manager, Admin                                    solana.PublicKey
	TotalValueRaw, LockedProfitDegradationSeconds, WithdrawalWaitingPeriodSeconds uint64
	ManagerPerformanceFeeBPS, AdminPerformanceFeeBPS, ManagerManagementFeeBPS     uint16
	AdminManagementFeeBPS, RedemptionFeeBPS, IssuanceFeeBPS                       uint16
	ProtocolPerformanceFeeBPS, ProtocolManagementFeeBPS                           uint16
	FeeAccumulatorManagerRaw, FeeAccumulatorAdminRaw, FeeAccumulatorProtocolRaw   uint64
	LPSupplyDeadWeightRaw                                                         uint64
	HighWaterMarkBits                                                             [16]byte // U80F48, little endian
	LastUpdatedLockedProfitRaw, LastLockedProfitReportUnix                        uint64
}

// DecodeVault decodes a Vault of at least the deployed size; bytes an
// upgrade appends are not read.
func DecodeVault(account *chain.Account) (Vault, error) {
	if !funded(account, VaultDiscriminator) || len(account.Data) < VaultSize {
		return Vault{}, errors.New("account is not a funded Voltr Vault of the deployed layout")
	}
	d := account.Data
	u16 := func(at int) uint16 { return binary.LittleEndian.Uint16(d[at:]) }
	u64 := func(at int) uint64 { return binary.LittleEndian.Uint64(d[at:]) }
	return Vault{
		AssetMint: key(d, 104), IdleATA: key(d, 136), LPMint: key(d, 272), Manager: key(d, 368), Admin: key(d, 400),
		TotalValueRaw: u64(168), LockedProfitDegradationSeconds: u64(448), WithdrawalWaitingPeriodSeconds: u64(456),
		ManagerPerformanceFeeBPS: u16(512), AdminPerformanceFeeBPS: u16(514), ManagerManagementFeeBPS: u16(516), AdminManagementFeeBPS: u16(518),
		RedemptionFeeBPS: u16(520), IssuanceFeeBPS: u16(522), ProtocolPerformanceFeeBPS: u16(524), ProtocolManagementFeeBPS: u16(526),
		FeeAccumulatorManagerRaw: u64(576), FeeAccumulatorAdminRaw: u64(584), FeeAccumulatorProtocolRaw: u64(592), LPSupplyDeadWeightRaw: u64(616),
		HighWaterMarkBits: [16]byte(d[624:640]), LastUpdatedLockedProfitRaw: u64(672), LastLockedProfitReportUnix: u64(680),
	}, nil
}

// StrategyReceipt is a StrategyInitReceipt: one strategy's position in a
// vault.
type StrategyReceipt struct {
	Vault, Strategy, AdaptorProgram solana.PublicKey
	PositionValueRaw, LastUpdatedTS uint64
	Version                         uint8
	// CustodyTrackedRaw is reserved (zero) on version 1 receipts; on the
	// upgraded binary it is the strategy custody balance Voltr books into the
	// vault's total value itself.
	CustodyTrackedRaw uint64
}

// DecodeStrategyReceipt decodes a StrategyInitReceipt of the deployed size
// whose reserved bytes, other than the custody balance, are zero.
func DecodeStrategyReceipt(account *chain.Account) (StrategyReceipt, error) {
	if !funded(account, StrategyReceiptDiscriminator) || len(account.Data) != StrategyReceiptSize {
		return StrategyReceipt{}, errors.New("account is not a funded Voltr StrategyInitReceipt of the deployed size")
	}
	d := account.Data
	if !zero(d[123:128]) || !zero(d[136:]) {
		return StrategyReceipt{}, errors.New("voltr strategy receipt reserved bytes are not zero")
	}
	return StrategyReceipt{
		Vault: key(d, 8), Strategy: key(d, 40), AdaptorProgram: key(d, 72),
		PositionValueRaw: binary.LittleEndian.Uint64(d[104:]), LastUpdatedTS: binary.LittleEndian.Uint64(d[112:]),
		Version: d[120], CustodyTrackedRaw: binary.LittleEndian.Uint64(d[128:]),
	}, nil
}

// WithdrawalReceipt is a RequestWithdrawVaultReceipt: one user's pending
// withdrawal.
type WithdrawalReceipt struct {
	Vault, User   solana.PublicKey
	LPEscrowedRaw uint64
	// AssetToWithdrawBits is the U80F48 asset amount as an integer of
	// 2^-48 units; UpperBoundAssetRaw is it rounded up to whole raw units.
	AssetToWithdrawBits *big.Int
	UpperBoundAssetRaw  uint64
	WithdrawableFromTS  uint64
	Bump, Version       uint8
}

// DecodeWithdrawalReceipt decodes a version 0 receipt of the deployed 112
// bytes (the SDK's 106-byte layout plus six zero bytes) that escrows LP, owes
// a positive asset amount that fits u64 and has a withdrawal deadline.
func DecodeWithdrawalReceipt(account *chain.Account) (WithdrawalReceipt, error) {
	if !funded(account, WithdrawalReceiptDiscriminator) || len(account.Data) != WithdrawalReceiptSize || !zero(account.Data[106:]) {
		return WithdrawalReceipt{}, errors.New("account is not a funded Voltr withdrawal receipt of the deployed layout")
	}
	d := account.Data
	little := make([]byte, 16)
	for i := range little {
		little[i] = d[95-i]
	}
	r := WithdrawalReceipt{
		Vault: key(d, 8), User: key(d, 40), LPEscrowedRaw: binary.LittleEndian.Uint64(d[72:]),
		AssetToWithdrawBits: new(big.Int).SetBytes(little), WithdrawableFromTS: binary.LittleEndian.Uint64(d[96:]), Bump: d[104], Version: d[105],
	}
	if r.LPEscrowedRaw == 0 || r.AssetToWithdrawBits.Sign() == 0 || r.WithdrawableFromTS == 0 || r.Version != 0 {
		return WithdrawalReceipt{}, errors.New("voltr withdrawal receipt has an invalid amount, deadline or version")
	}
	one := new(big.Int).Lsh(big.NewInt(1), 48)
	upper := new(big.Int).Add(r.AssetToWithdrawBits, new(big.Int).Sub(one, big.NewInt(1)))
	if upper.Rsh(upper, 48); !upper.IsUint64() {
		return WithdrawalReceipt{}, errors.New("voltr withdrawal receipt amount overflows u64")
	}
	r.UpperBoundAssetRaw = upper.Uint64()
	return r, nil
}

// WithdrawalReceiptAddress derives ["request_withdraw_vault_receipt", vault,
// user].
func WithdrawalReceiptAddress(vault, user solana.PublicKey) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("request_withdraw_vault_receipt"), vault[:], user[:]}, ProgramID)
}

// WithdrawalReceiptFilters select every withdrawal receipt of vault in a
// getProgramAccounts scan of ProgramID.
func WithdrawalReceiptFilters(vault solana.PublicKey) []rpc.RPCFilter {
	return []rpc.RPCFilter{
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: WithdrawalReceiptDiscriminator[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: vault[:]}},
	}
}

// Strategy is the fixed account set of deposit_strategy and
// withdraw_strategy; the adaptor's own accounts follow it as remaining
// accounts. Each instruction's account order is written once, as a slot list
// over the slot type: the builder reads keys from it, *Allowed reads the
// squads.Slot each position admits.
type Strategy[T any] struct {
	Manager, Protocol, Vault, Strategy, AdaptorAddReceipt, StrategyInitReceipt  T
	VaultAssetIdleAuth, VaultStrategyAuth, AssetMint, LPMint                    T
	VaultAssetIdleATA, VaultStrategyAssetATA, AssetTokenProgram, AdaptorProgram T
}

type (
	StrategyAccounts = Strategy[solana.PublicKey]
	StrategyAllowed  = Strategy[squads.Slot]
)

// Offsets in deposit_strategy and withdraw_strategy data: the amount, then
// the adaptor call, Some(adaptor instruction) and Some(adaptor args), which
// Voltr forwards to the adaptor.
const (
	StrategyAmountOffset      = 8
	StrategyAdaptorCallOffset = 16
)

func depositStrategySlots[T any](a Strategy[T]) []slot[T] {
	return []slot[T]{
		signer(a.Manager), readonly(a.Protocol), writable(a.Vault), readonly(a.Strategy),
		readonly(a.AdaptorAddReceipt), writable(a.StrategyInitReceipt), writable(a.VaultAssetIdleAuth),
		writable(a.VaultStrategyAuth), writable(a.AssetMint), readonly(a.LPMint), writable(a.VaultAssetIdleATA),
		writable(a.VaultStrategyAssetATA), readonly(a.AssetTokenProgram), readonly(a.AdaptorProgram),
	}
}

func withdrawStrategySlots[T any](a Strategy[T]) []slot[T] {
	return []slot[T]{
		signer(a.Manager), readonly(a.Protocol), writable(a.Vault), readonly(a.AdaptorAddReceipt),
		writable(a.StrategyInitReceipt), readonly(a.Strategy), readonly(a.AdaptorProgram), writable(a.VaultAssetIdleAuth),
		writable(a.VaultStrategyAuth), writable(a.AssetMint), readonly(a.LPMint), writable(a.VaultAssetIdleATA),
		writable(a.VaultStrategyAssetATA), readonly(a.AssetTokenProgram),
	}
}

// DepositStrategy moves amount of the vault's idle asset into a strategy
// through its adaptor. adaptorInstruction is the adaptor discriminator Voltr
// calls; adaptorArgs, when not nil, are the bytes it forwards to it.
func DepositStrategy(a StrategyAccounts, amount uint64, adaptorInstruction, adaptorArgs []byte, remaining ...*solana.AccountMeta) *solana.GenericInstruction {
	return strategyInstruction(DepositStrategyDiscriminator, amount, adaptorInstruction, adaptorArgs, append(metas(depositStrategySlots(a)), remaining...))
}

// DepositStrategyAllowed admits DepositStrategy over the allowed accounts,
// then the adaptor's remaining accounts, with data predicates after its
// discriminator.
func DepositStrategyAllowed(a StrategyAllowed, remaining []squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(DepositStrategyDiscriminator, depositStrategySlots(a), remaining, data)
}

// WithdrawStrategy moves amount of the vault's asset out of a strategy back
// to idle through its adaptor, with DepositStrategy's arguments.
func WithdrawStrategy(a StrategyAccounts, amount uint64, adaptorInstruction, adaptorArgs []byte, remaining ...*solana.AccountMeta) *solana.GenericInstruction {
	return strategyInstruction(WithdrawStrategyDiscriminator, amount, adaptorInstruction, adaptorArgs, append(metas(withdrawStrategySlots(a)), remaining...))
}

// WithdrawStrategyAllowed admits WithdrawStrategy as DepositStrategyAllowed
// admits DepositStrategy.
func WithdrawStrategyAllowed(a StrategyAllowed, remaining []squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(WithdrawStrategyDiscriminator, withdrawStrategySlots(a), remaining, data)
}

// slot is one account position of an instruction: a key (or what the policy
// admits there) and the position's flags.
type slot[T any] struct {
	key              T
	writable, signer bool
}

func readonly[T any](k T) slot[T] { return slot[T]{key: k} }
func writable[T any](k T) slot[T] { return slot[T]{key: k, writable: true} }
func signer[T any](k T) slot[T]   { return slot[T]{key: k, signer: true} }

func metas(slots []slot[solana.PublicKey]) []*solana.AccountMeta {
	out := make([]*solana.AccountMeta, len(slots))
	for i, s := range slots {
		out[i] = &solana.AccountMeta{PublicKey: s.key, IsWritable: s.writable, IsSigner: s.signer}
	}
	return out
}

func allow(discriminator [8]byte, slots []slot[squads.Slot], remaining []squads.Slot, data []squads.DataConstraintView) squads.InstructionConstraintView {
	allowed := make([]squads.Slot, 0, len(slots)+len(remaining))
	for _, s := range slots {
		allowed = append(allowed, s.key)
	}
	predicates := append([]squads.DataConstraintView{squads.DataBytes(0, discriminator[:])}, data...)
	return squads.AllowData(ProgramID, predicates, append(allowed, remaining...))
}

// strategyInstruction encodes (amount: u64, instruction_discriminator:
// Option<Vec<u8>>, additional_args: Option<Vec<u8>>).
func strategyInstruction(discriminator [8]byte, amount uint64, adaptorInstruction, adaptorArgs []byte, accounts []*solana.AccountMeta) *solana.GenericInstruction {
	data := binary.LittleEndian.AppendUint64(discriminator[:], amount)
	for _, option := range [][]byte{adaptorInstruction, adaptorArgs} {
		if option == nil {
			data = append(data, 0)
			continue
		}
		data = append(binary.LittleEndian.AppendUint32(append(data, 1), uint32(len(option))), option...)
	}
	return solana.NewInstruction(ProgramID, accounts, data)
}

// funded reports whether account is a live, funded Voltr account of the
// discriminated type.
func funded(account *chain.Account, discriminator [8]byte) bool {
	return account != nil && account.Owner == ProgramID && !account.Executable && account.Lamports > 0 && bytes.HasPrefix(account.Data, discriminator[:])
}

func key(data []byte, at int) solana.PublicKey { return solana.PublicKeyFromBytes(data[at : at+32]) }

func zero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
