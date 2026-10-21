package backyard

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

var ErrTransactionConstructionUnavailable = fmt.Errorf("transaction construction blocked: deployed adaptor v2 and complete policy catalog are required")

type publicKey [32]byte

type accountMeta struct {
	key      publicKey
	signer   bool
	writable bool
}

type compiledInstruction struct {
	program  publicKey
	accounts []accountMeta
	data     []byte
}

// BridgeReport is the fixed payload accepted by the immutable adaptor v2.
// SnapshotDigest must be ComputeNAV's lower-case hex digest.
type BridgeReport struct {
	Sequence       uint64
	ObservedSlot   uint64
	NAVAfterRaw    uint64
	SnapshotDigest string
}

// BridgeBuildRequest is intentionally closed over the four bridge operations
// which can run before the Kamino open packet is ready.  The caller supplies a
// confirmed recent blockhash and the exact delegated executor private key; no
// environment/key file is read by this package.
type BridgeBuildRequest struct {
	Action    Action
	AmountRaw uint64
	Report    BridgeReport
	Policy    string
	// These bindings are read from the confirmed immutable adaptor config and
	// Squads Settings before a transaction is built. They are repeated here so
	// a stale observation cannot be silently paired with the hard-coded wire.
	AdaptorConfig        string
	Settings             string
	RecentBlockhash      string
	LastValidBlockHeight int64
}

type SignedTransaction struct {
	message              []byte
	signedWire           []byte
	messageSHA256        string
	signedWireSHA256     string
	transactionSignature string
	recentBlockhash      string
	lastValidBlockHeight int64
}

func signMessage(executor ed25519.PrivateKey, expectedDelegate publicKey, message []byte, blockhash string, height int64) (SignedTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || height <= 0 {
		return SignedTransaction{}, fmt.Errorf("invalid signing material")
	}
	if publicKeyFromBytes(executor.Public().(ed25519.PublicKey)) != expectedDelegate {
		return SignedTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	signature := ed25519.Sign(executor, message)
	wire := signedWire(message, signature)
	if len(wire) > maxTransactionBytes {
		return SignedTransaction{}, fmt.Errorf("transaction is %d bytes, exceeds %d", len(wire), maxTransactionBytes)
	}
	return SignedTransaction{message: message, signedWire: wire, messageSHA256: sha256Bytes(message), signedWireSHA256: sha256Bytes(wire),
		transactionSignature: encodeBase58(signature), recentBlockhash: blockhash, lastValidBlockHeight: height}, nil
}

// Backyard's Squads Settings, its one signer, the delegate every Backyard
// policy is delegated to, and USDC, the asset of every vault. A vault's own
// accounts are derived from its data (vault.go).
const (
	bridgeSettings       = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"
	bridgeSettingsSigner = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"
	bridgeDelegate       = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
	bridgeUSDC           = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

var (
	adaptorDepositDiscriminator  = []byte{242, 35, 198, 137, 82, 225, 242, 182}
	adaptorWithdrawDiscriminator = []byte{183, 18, 70, 156, 148, 109, 161, 34}
)

// bridgeCapRaw stays the whole-vault bound used by the Kamino basic policies
// and Voltr withdrawal-receipt admission. bridgeMaxNAV is the strategy-two
// adaptor config's max reported NAV (the vault maxCap) decoded from confirmed
// config state. strategyTwoBridgeLegCapRaw is the per-execution operational
// bound installed on bridge policies 152-155 (200k USDC): every bridge capital
// leg is built against it. The daily USDC spending limit embedded in those
// policies is Squads' alone: simulation refuses an over-limit wire before
// broadcast (squadsSpendingLimitReason).
const (
	bridgeCapRaw uint64 = 1_000_000_000_000
	bridgeMaxNAV uint64 = 1_000_000_000_000

	strategyTwoBridgeLegCapRaw uint64 = 200_000_000_000
)

func compileBridgeMessageForDelegate(v *Vault, request BridgeBuildRequest, delegate publicKey) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 || request.AdaptorConfig != v.Strategy || request.Settings != bridgeSettings || request.Report.Sequence != request.Report.ObservedSlot {
		return nil, fmt.Errorf("bridge config or report is not bound to confirmed state")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	bridgeInstructions := ticketedBridgeInstructions
	if v.trustful() {
		bridgeInstructions = trustfulBridgeInstructions
	}
	inner, indexes, err := bridgeInstructions(v, request)
	if err != nil {
		return nil, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil {
		return nil, fmt.Errorf("bridge policy: %w", err)
	}
	outer, err := wrapSquadsPolicyForDelegate(v, policy, delegate, delegate, indexes, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, []compiledInstruction{outer}, executionBudget("", 1))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func bridgeInstruction(v *Vault, request BridgeBuildRequest) (compiledInstruction, error) {
	switch request.Action {
	case VoltrAllocateToSquads:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid allocation amount")
		}
		return voltrStrategyInstruction(v, voltr.DepositStrategy, adaptorDepositDiscriminator, request.AmountRaw, request.Report)
	case ReportNAV:
		if request.AmountRaw != 0 {
			return compiledInstruction{}, fmt.Errorf("NAV refresh cannot move capital")
		}
		return voltrStrategyInstruction(v, voltr.DepositStrategy, adaptorDepositDiscriminator, 0, request.Report)
	case VoltrRestoreIdle:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid Voltr restore amount")
		}
		return voltrStrategyInstruction(v, voltr.WithdrawStrategy, adaptorWithdrawDiscriminator, request.AmountRaw, request.Report)
	case StageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid staging amount")
		}
		return stageInstruction(v, request.AmountRaw), nil
	default:
		return compiledInstruction{}, fmt.Errorf("action %s has no approved bridge transaction", request.Action)
	}
}

func voltrStrategyInstruction(v *Vault, build func(voltr.StrategyAccounts, uint64, []byte, []byte, ...squads.AccountSlot[solana.PublicKey]) *solana.GenericInstruction,
	adaptorDiscriminator []byte, amount uint64, report BridgeReport) (compiledInstruction, error) {
	encodedReport, err := encodeBridgeReport(report)
	if err != nil {
		return compiledInstruction{}, err
	}
	remaining := bridgeRemainingSlots(bridgeRemaining[solana.PublicKey]{Settings: solanaKey(bridgeSettings), Vault: solanaKey(v.squads),
		Custody: solanaKey(v.cash), Ticket: solanaKey(reportTicketPDA)})
	accounts := v.strategyAccounts()
	return sdkInstruction(build(accounts, amount, adaptorDiscriminator, encodedReport, remaining...)), nil
}

type bridgeRemaining[T any] struct {
	Settings, Vault, Custody, Ticket T
}

func bridgeRemainingSlots[T any](r bridgeRemaining[T]) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{squads.ReadOnly(r.Settings), squads.Signing(r.Vault), squads.Writable(r.Custody), squads.Writable(r.Ticket)}
}

// The adaptor report both bridge legs carry as Voltr's additional_args,
// Some(Vec<u8>): the option tag and u32 length, then ReportV1, which is its
// version, sequence, observed slot, NAV after and snapshot digest.
const (
	bridgeReportArgsPrefixLen = 1 + 4
	bridgeReportVersion       = 1
	bridgeReportNAVOffset     = 1 + 8 + 8
	bridgeReportLen           = bridgeReportNAVOffset + 8 + 32
)

func encodeBridgeReport(report BridgeReport) ([]byte, error) {
	if report.Sequence == 0 || report.Sequence != report.ObservedSlot || report.NAVAfterRaw > bridgeMaxNAV {
		return nil, fmt.Errorf("invalid adaptor report fields")
	}
	digest, err := hex.DecodeString(report.SnapshotDigest)
	if err != nil || len(digest) != 32 || allZero(digest) {
		return nil, fmt.Errorf("invalid adaptor snapshot digest")
	}
	data := []byte{bridgeReportVersion}
	data = appendU64(data, report.Sequence)
	data = appendU64(data, report.ObservedSlot)
	data = appendU64(data, report.NAVAfterRaw)
	return append(data, digest...), nil
}

const (
	trustfulDepositLeg = iota
	trustfulDepositLegs
)

const (
	trustfulHoldLeg = iota
	trustfulWithdrawLeg
	trustfulWithdrawLegs
)

func trustfulBridgeInstructions(v *Vault, request BridgeBuildRequest) ([]compiledInstruction, []byte, error) {
	book := request.Report.NAVAfterRaw
	if book > bridgeMaxNAV {
		return nil, nil, fmt.Errorf("Trustful end value exceeds the vault cap")
	}
	accounts := v.strategyAccounts()
	switch request.Action {
	case VoltrAllocateToSquads, ReportNAV:
		if (request.Action == ReportNAV) != (request.AmountRaw == 0) || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return nil, nil, fmt.Errorf("invalid Trustful deposit amount")
		}
		deposit := voltr.TrustfulDeposit(accounts, request.AmountRaw, book, solanaKey(v.cash))
		return []compiledInstruction{sdkInstruction(deposit)}, []byte{trustfulDepositLeg}, nil
	case StageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return nil, nil, fmt.Errorf("invalid Trustful withdrawal amount")
		}
		hold := spl.TransferChecked(solana.TokenProgramID, solanaKey(v.cash), solanaKey(bridgeUSDC), solanaKey(v.holding), solanaKey(v.squads), request.AmountRaw, 6)
		withdraw := voltr.TrustfulWithdraw(accounts, request.AmountRaw, book, solanaKey(v.holdingAuth), solanaKey(v.holding))
		return []compiledInstruction{sdkInstruction(hold), sdkInstruction(withdraw)}, []byte{trustfulHoldLeg, trustfulWithdrawLeg}, nil
	}
	return nil, nil, fmt.Errorf("action %s has no Trustful bridge transaction", request.Action)
}

func stageInstruction(v *Vault, amount uint64) compiledInstruction {
	return sdkInstruction(spl.TransferChecked(solana.TokenProgramID, solanaKey(v.cash), solanaKey(bridgeUSDC), solanaKey(v.strategyATA), solanaKey(v.squads), amount, 6))
}

func solanaKey(value string) solana.PublicKey { return solana.PublicKey(mustKey(value)) }

func (ix compiledInstruction) squads() squads.Instruction {
	out := squads.Instruction{ProgramID: solana.PublicKey(ix.program), Data: ix.data}
	for _, account := range ix.accounts {
		out.Accounts = append(out.Accounts, solana.AccountMeta{PublicKey: solana.PublicKey(account.key), IsSigner: account.signer, IsWritable: account.writable})
	}
	return out
}

func sdkInstruction(ix *solana.GenericInstruction) compiledInstruction {
	out := compiledInstruction{program: publicKey(ix.ProgID), data: ix.DataBytes}
	for _, account := range ix.AccountValues {
		out.accounts = append(out.accounts, accountMeta{key: publicKey(account.PublicKey), signer: account.IsSigner, writable: account.IsWritable})
	}
	return out
}

func wrapSquadsPolicyForDelegate(v *Vault, policy, executor, expectedDelegate publicKey, constraintIndexes []byte, inner []compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads bridge delegate")
	}
	return wrapSquadsPolicy(v, policy, executor, constraintIndexes, inner...)
}

func wrapSquadsPolicy(v *Vault, policy, executor publicKey, constraintIndexes []byte, inner ...compiledInstruction) (compiledInstruction, error) {
	execute := squads.ExecuteSync{Policy: solana.PublicKey(policy), Signer: solana.PublicKey(executor), AccountIndex: v.SquadsIndex,
		ConstraintIndexes: constraintIndexes}
	for _, ix := range inner {
		execute.Inner = append(execute.Inner, ix.squads())
	}
	wrapped, err := squads.ExecuteTransactionSyncV2(execute)
	if err != nil {
		return compiledInstruction{}, err
	}
	out := compiledInstruction{program: publicKey(wrapped.ProgramID), data: wrapped.Data}
	for _, account := range wrapped.Accounts {
		out.accounts = append(out.accounts, accountMeta{key: publicKey(account.PublicKey), signer: account.IsSigner, writable: account.IsWritable})
	}
	return out, nil
}

func accountRank(account accountMeta) int {
	if account.signer {
		if account.writable {
			return 0
		}
		return 1
	}
	if account.writable {
		return 2
	}
	return 3
}
func pushOrMergeMeta(accounts *[]accountMeta, next accountMeta) byte {
	for i := range *accounts {
		if (*accounts)[i].key == next.key {
			(*accounts)[i].signer = (*accounts)[i].signer || next.signer
			(*accounts)[i].writable = (*accounts)[i].writable || next.writable
			return byte(i)
		}
	}
	*accounts = append(*accounts, next)
	return byte(len(*accounts) - 1)
}
func mustKey(value string) publicKey {
	key, err := decodeKey(value)
	if err != nil {
		panic("invalid checked-in bridge identity: " + value)
	}
	return key
}
func publicKeyFromBytes(value []byte) publicKey { var key publicKey; copy(key[:], value); return key }
func appendU64(dst []byte, value uint64) []byte {
	for i := 0; i < 8; i++ {
		dst = append(dst, byte(value))
		value >>= 8
	}
	return dst
}
func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func decodeKey(value string) (publicKey, error) {
	raw, err := decodeBase58(value)
	if err != nil || len(raw) != 32 {
		return publicKey{}, fmt.Errorf("not a 32-byte base58 value")
	}
	var key publicKey
	copy(key[:], raw)
	return key, nil
}
func decodeBase58(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("empty base58")
	}
	bytes := []byte{0}
	for _, char := range []byte(value) {
		digit := int64(-1)
		for i := 0; i < len(base58Alphabet); i++ {
			if base58Alphabet[i] == char {
				digit = int64(i)
				break
			}
		}
		if digit < 0 {
			return nil, fmt.Errorf("invalid base58 character")
		}
		carry := digit
		for i := len(bytes) - 1; i >= 0; i-- {
			carry += int64(bytes[i]) * 58
			bytes[i] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			bytes = append([]byte{byte(carry)}, bytes...)
			carry >>= 8
		}
	}
	zeros := 0
	for zeros < len(value) && value[zeros] == '1' {
		zeros++
	}
	// The accumulator starts at zero. Strip that sentinel even when the input
	// is entirely leading-zero digits (for example Solana's system program,
	// 11111111111111111111111111111111), then restore exactly the encoded
	// number of leading zero bytes below.
	for len(bytes) > 0 && bytes[0] == 0 {
		bytes = bytes[1:]
	}
	return append(make([]byte, zeros), bytes...), nil
}
func encodeBase58(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digits := []byte{0}
	for _, b := range value {
		carry := int(b)
		for i := len(digits) - 1; i >= 0; i-- {
			carry += int(digits[i]) << 8
			digits[i] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append([]byte{byte(carry % 58)}, digits...)
			carry /= 58
		}
	}
	zeros := 0
	for zeros < len(value) && value[zeros] == 0 {
		zeros++
	}
	out := make([]byte, zeros, zeros+len(digits))
	for i := range out {
		out[i] = base58Alphabet[0]
	}
	if zeros == len(value) {
		return string(out)
	}
	for _, digit := range digits {
		out = append(out, base58Alphabet[digit])
	}
	return string(out)
}
func kaminoLegDiscriminator(leg kaminoPrimeUSDCLeg) []byte {
	switch leg {
	case kaminoLegDeposit:
		return kamino.DepositV2Discriminator[:]
	case kaminoLegBorrow:
		return kamino.BorrowV2Discriminator[:]
	case kaminoLegRepay:
		return kamino.RepayV2Discriminator[:]
	case kaminoLegWithdraw:
		return kamino.WithdrawV2Discriminator[:]
	default:
		return nil
	}
}
