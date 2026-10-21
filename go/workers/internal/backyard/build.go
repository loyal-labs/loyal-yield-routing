package backyard

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/mr-tron/base58"
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
type BridgeReport struct {
	Sequence       uint64
	ObservedSlot   uint64
	NAVAfterRaw    uint64
	SnapshotDigest navDigest
}

type BridgeAction Action

const (
	bridgeAllocateToSquads   = BridgeAction(VoltrAllocateToSquads)
	bridgeStageSquadsToVoltr = BridgeAction(StageSquadsToVoltr)
	bridgeRestoreIdle        = BridgeAction(VoltrRestoreIdle)
	bridgeReportNAV          = BridgeAction(ReportNAV)
)

type BridgeBuildRequest struct {
	Action               BridgeAction
	AmountRaw            uint64
	Report               BridgeReport
	Policy               string
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

const (
	backyardSettings       = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"
	backyardSettingsSigner = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"
	backyardDelegate       = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
)

var (
	adaptorDepositDiscriminator  = []byte{242, 35, 198, 137, 82, 225, 242, 182}
	adaptorWithdrawDiscriminator = []byte{183, 18, 70, 156, 148, 109, 161, 34}
)

const (
	bridgeCapRaw               uint64 = 1_000_000_000_000
	bridgeMaxNAV               uint64 = 1_000_000_000_000
	bridgeDailyCapRaw          uint64 = 200_000_000_000
	strategyTwoBridgeLegCapRaw uint64 = 200_000_000_000
)

func compileBridgeMessageForDelegate(v *Vault, request BridgeBuildRequest, delegate publicKey) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 || request.Report.Sequence != request.Report.ObservedSlot {
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
	case bridgeAllocateToSquads:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid allocation amount")
		}
		return voltrStrategyInstruction(v, voltr.DepositStrategy, adaptorDepositDiscriminator, request.AmountRaw, request.Report)
	case bridgeReportNAV:
		if request.AmountRaw != 0 {
			return compiledInstruction{}, fmt.Errorf("NAV refresh cannot move capital")
		}
		return voltrStrategyInstruction(v, voltr.DepositStrategy, adaptorDepositDiscriminator, 0, request.Report)
	case bridgeRestoreIdle:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid Voltr restore amount")
		}
		return voltrStrategyInstruction(v, voltr.WithdrawStrategy, adaptorWithdrawDiscriminator, request.AmountRaw, request.Report)
	case bridgeStageSquadsToVoltr:
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
	remaining := bridgeRemainingSlots(bridgeRemaining[solana.PublicKey]{Settings: solanaKey(backyardSettings), Vault: solanaKey(v.squads),
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
	digest, err := hex.DecodeString(string(report.SnapshotDigest))
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
	case bridgeAllocateToSquads, bridgeReportNAV:
		if (request.Action == bridgeReportNAV) != (request.AmountRaw == 0) || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return nil, nil, fmt.Errorf("invalid Trustful deposit amount")
		}
		deposit := voltr.TrustfulDeposit(accounts, request.AmountRaw, book, solanaKey(v.cash))
		return []compiledInstruction{sdkInstruction(deposit)}, []byte{trustfulDepositLeg}, nil
	case bridgeStageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return nil, nil, fmt.Errorf("invalid Trustful withdrawal amount")
		}
		hold := spl.TransferChecked(solana.TokenProgramID, solanaKey(v.cash), solanaKey(fleet.USDCMint), solanaKey(v.holding), solanaKey(v.squads), request.AmountRaw, 6)
		withdraw := voltr.TrustfulWithdraw(accounts, request.AmountRaw, book, solanaKey(v.holdingAuth), solanaKey(v.holding))
		return []compiledInstruction{sdkInstruction(hold), sdkInstruction(withdraw)}, []byte{trustfulHoldLeg, trustfulWithdrawLeg}, nil
	}
	return nil, nil, fmt.Errorf("action %s has no Trustful bridge transaction", request.Action)
}

func stageInstruction(v *Vault, amount uint64) compiledInstruction {
	return sdkInstruction(spl.TransferChecked(solana.TokenProgramID, solanaKey(v.cash), solanaKey(fleet.USDCMint), solanaKey(v.strategyATA), solanaKey(v.squads), amount, 6))
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

func decodeKey(value string) (publicKey, error) {
	raw, err := decodeBase58(value)
	if err != nil || len(raw) != 32 {
		return publicKey{}, fmt.Errorf("not a 32-byte base58 value")
	}
	var key publicKey
	copy(key[:], raw)
	return key, nil
}

var (
	decodeBase58 = base58.Decode
	encodeBase58 = base58.Encode
)

func kaminoLegDiscriminator(leg kaminoLeg) []byte {
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
