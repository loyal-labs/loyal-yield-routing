package backyard

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

var ErrTransactionConstructionUnavailable = fmt.Errorf("transaction construction blocked: deployed adaptor v2 and complete policy catalog are required")

// This file deliberately implements only the legacy Solana message encoding
// needed by the Backyard worker.  It is not a general Solana SDK.  Keeping the
// codec here makes the exact bytes that are simulated, persisted, and later
// recovered independently reviewable, without a Rust or TypeScript runtime.

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
	// Policy is the installed account of the action's bridge policy.
	Policy string
	// These bindings are read from the confirmed immutable adaptor config and
	// Squads Settings before a transaction is built. They are repeated here so
	// a stale observation cannot be silently paired with the hard-coded wire.
	AdaptorConfig        string
	Settings             string
	RecentBlockhash      string
	LastValidBlockHeight int64
}

// SignedTransaction is one exact signed wire (message, then signature). It is
// suitable for sig-verified RPC simulation, but cannot be persisted as a
// BuildResult until that simulation returns its slot.
type SignedTransaction struct {
	message              []byte
	signedWire           []byte
	messageSHA256        string
	signedWireSHA256     string
	transactionSignature string
	recentBlockhash      string
	lastValidBlockHeight int64
}

// signMessage signs message as executor, which must be expectedDelegate:
// the one wire every Backyard build persists.
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

// BuildResult attaches the only post-signing datum: the slot returned from
// simulating this exact wire.  This prevents callers from accidentally
// persisting an unsigned, re-built, or differently simulated transaction.
func (s SignedTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 || s.transactionSignature == "" {
		return BuildResult{}, fmt.Errorf("exact signed transaction was not simulated")
	}
	result := BuildResult{
		MessageSHA256:        s.messageSHA256,
		SignedWire:           append([]byte(nil), s.signedWire...),
		SignedWireSHA256:     s.signedWireSHA256,
		TransactionSignature: s.transactionSignature,
		RecentBlockhash:      s.recentBlockhash,
		LastValidBlockHeight: s.lastValidBlockHeight,
		SimulationSlot:       simulationSlot,
	}
	return result, result.Validate()
}

// Route identities are copied from the checked-in v4 RWA manifest/route spec.
// They are not configurable: a different key is a different reviewed manifest,
// not an environment override.
const (
	bridgeSettings       = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"
	bridgeSettingsSigner = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"
	bridgeVault          = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	bridgeDelegate       = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
	bridgeVoltrVault     = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"
	// Strategy-two adaptor config: its key IS the Voltr strategy key, derived
	// offline from the setup admin over domain loyal-rwa-multiply-mainnet-v3.
	// It replaces the retired v2 config 9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj.
	bridgeStrategy        = "DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY"
	bridgeProtocol        = "4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y"
	bridgeAdaptorReceipt  = "AsfkxMdVYjMnr2fdTBMUXhq81hgi2hbENXCy9WhUQF7u"
	bridgeStrategyReceipt = "5bw4VYzpZXsk9SUNyWwJkb4fEx1DS8eNMFB6Qb4MUfhE"
	bridgeIdleAuthority   = "EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb"
	bridgeStrategyAuth    = "5r74AE7yewacfRzoGAjXx5X3gM9LUoLU29eHzdjiLrJo"
	bridgeUSDC            = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	bridgeLPMint          = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"
	bridgeIdleATA         = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"
	bridgeStrategyATA     = "EPCVCLY5wfumf6yPvqu7zuEB4WnnXbnPsy7JrKoAWcqC"
	bridgeSquadsATA       = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"
	bridgeAdaptorProgram  = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"
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

// BuildAndSignBridgeTransaction builds exactly one policy-wrapped bridge
// operation. Capital/NAV payloads contain the atomic ArmReport -> Voltr pair;
// staging remains one SPL instruction. The only top-level signer is the pinned
// delegated executor and the Squads vault is signer only for inner execution.
func BuildAndSignBridgeTransaction(request BridgeBuildRequest, executor ed25519.PrivateKey) (SignedTransaction, error) {
	return buildAndSignBridgeTransactionForDelegate(request, executor, mustKey(bridgeDelegate))
}

// CompileBridgeMessage uses the exact signing path without accessing a key.
// Fee/cap admission therefore measures the message before signing is possible.
func CompileBridgeMessage(request BridgeBuildRequest) ([]byte, error) {
	return compileBridgeMessageForDelegate(request, mustKey(bridgeDelegate))
}

func compileBridgeMessageForDelegate(request BridgeBuildRequest, delegate publicKey) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 || request.AdaptorConfig != bridgeStrategy || request.Settings != bridgeSettings || request.Report.Sequence != request.Report.ObservedSlot {
		return nil, fmt.Errorf("bridge config or report is not bound to confirmed state")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, indexes, err := ticketedBridgeInstructions(request)
	if err != nil {
		return nil, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil {
		return nil, fmt.Errorf("bridge policy: %w", err)
	}
	outer, err := wrapSquadsPolicyForDelegate(policy, delegate, delegate, indexes, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, []compiledInstruction{outer}, executionBudget("", 1))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// buildAndSignBridgeTransactionForDelegate exists solely so package tests can
// verify Solana wire encoding with deterministic non-production key material.
// Production always calls BuildAndSignBridgeTransaction, which pins the real
// delegated executor above.
func buildAndSignBridgeTransactionForDelegate(request BridgeBuildRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedTransaction, error) {
	message, err := compileBridgeMessageForDelegate(request, expectedDelegate)
	if err != nil {
		return SignedTransaction{}, err
	}
	return signMessage(executor, expectedDelegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
}

func bridgeInstruction(request BridgeBuildRequest) (compiledInstruction, error) {
	switch request.Action {
	case VoltrAllocateToSquads:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid allocation amount")
		}
		return voltrStrategyInstruction(voltr.DepositStrategy, adaptorDepositDiscriminator, request.AmountRaw, request.Report)
	case ReportNAV:
		if request.AmountRaw != 0 {
			return compiledInstruction{}, fmt.Errorf("NAV refresh cannot move capital")
		}
		return voltrStrategyInstruction(voltr.DepositStrategy, adaptorDepositDiscriminator, 0, request.Report)
	case VoltrRestoreIdle:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid Voltr restore amount")
		}
		return voltrStrategyInstruction(voltr.WithdrawStrategy, adaptorWithdrawDiscriminator, request.AmountRaw, request.Report)
	case StageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, fmt.Errorf("invalid staging amount")
		}
		return stageInstruction(request.AmountRaw), nil
	default:
		return compiledInstruction{}, fmt.Errorf("action %s has no approved bridge transaction", request.Action)
	}
}

// voltrStrategyInstruction is a bridge Voltr deposit_strategy or
// withdraw_strategy: Voltr calls the pinned adaptor instruction with the
// encoded report, over the bridge's remaining accounts.
func voltrStrategyInstruction(build func(voltr.StrategyAccounts, uint64, []byte, []byte, ...squads.AccountSlot[solana.PublicKey]) *solana.GenericInstruction,
	adaptorDiscriminator []byte, amount uint64, report BridgeReport) (compiledInstruction, error) {
	encodedReport, err := encodeBridgeReport(report)
	if err != nil {
		return compiledInstruction{}, err
	}
	accounts := voltr.StrategyAccounts{Manager: solanaKey(bridgeVault), Protocol: solanaKey(bridgeProtocol), Vault: solanaKey(bridgeVoltrVault),
		Strategy: solanaKey(bridgeStrategy), AdaptorAddReceipt: solanaKey(bridgeAdaptorReceipt), StrategyInitReceipt: solanaKey(bridgeStrategyReceipt),
		VaultAssetIdleAuth: solanaKey(bridgeIdleAuthority), VaultStrategyAuth: solanaKey(bridgeStrategyAuth), AssetMint: solanaKey(bridgeUSDC),
		LPMint: solanaKey(bridgeLPMint), VaultAssetIdleATA: solanaKey(bridgeIdleATA), VaultStrategyAssetATA: solanaKey(bridgeStrategyATA),
		AssetTokenProgram: solanaKey(bridgeTokenProgram), AdaptorProgram: solanaKey(bridgeAdaptorProgram)}
	remaining := bridgeRemainingSlots(bridgeRemaining[solana.PublicKey]{Settings: solanaKey(bridgeSettings), Vault: solanaKey(bridgeVault),
		Custody: solanaKey(bridgeSquadsATA), Ticket: solanaKey(reportTicketPDA)})
	return sdkInstruction(build(accounts, amount, adaptorDiscriminator, encodedReport, remaining...)), nil
}

// bridgeRemaining is the adaptor's remaining accounts after Voltr's own, as
// both the bridge's Voltr legs and their policy constraints read them
// (bridgeRemainingSlots): the Squads settings, the vault that signs, its USDC
// custody, then the report ticket the adaptor consumes.
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

func stageInstruction(amount uint64) compiledInstruction {
	return sdkInstruction(spl.TransferChecked(solana.TokenProgramID, solanaKey(bridgeSquadsATA), solanaKey(bridgeUSDC), solanaKey(bridgeStrategyATA), solanaKey(bridgeVault), amount, 6))
}

func solanaKey(value string) solana.PublicKey { return solana.PublicKey(mustKey(value)) }

// squads is ix as Squads reads an inner instruction.
func (ix compiledInstruction) squads() squads.Instruction {
	out := squads.Instruction{ProgramID: solana.PublicKey(ix.program), Data: ix.data}
	for _, account := range ix.accounts {
		out.Accounts = append(out.Accounts, solana.AccountMeta{PublicKey: solana.PublicKey(account.key), IsSigner: account.signer, IsWritable: account.writable})
	}
	return out
}

// sdkInstruction is an instruction solana-go built.
func sdkInstruction(ix *solana.GenericInstruction) compiledInstruction {
	out := compiledInstruction{program: publicKey(ix.ProgID), data: ix.DataBytes}
	for _, account := range ix.AccountValues {
		out.accounts = append(out.accounts, accountMeta{key: publicKey(account.PublicKey), signer: account.IsSigner, writable: account.IsWritable})
	}
	return out
}

func wrapSquadsPolicyForDelegate(policy, executor, expectedDelegate publicKey, constraintIndexes []byte, inner []compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads bridge delegate")
	}
	return wrapSquadsPolicy(policy, executor, constraintIndexes, inner...)
}

// wrapSquadsPolicy runs inner as the bridge vault (backyardVaultIndex)
// through policy, signed by executor.
func wrapSquadsPolicy(policy, executor publicKey, constraintIndexes []byte, inner ...compiledInstruction) (compiledInstruction, error) {
	execute := squads.ExecuteSync{Policy: solana.PublicKey(policy), Signer: solana.PublicKey(executor), AccountIndex: backyardVaultIndex,
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
func (b BuildResult) Validate() error {
	return b.validateForDelegate(publicKey{})
}

// validateForDelegate additionally pins the sole top-level signer when a
// nonzero expectedDelegate is supplied. PersistSigned uses the production
// delegate; tests may use deterministic local keys to exercise the codec.
func (b BuildResult) validateForDelegate(expectedDelegate publicKey) error {
	if len(b.SignedWire) == 0 || b.TransactionSignature == "" || b.RecentBlockhash == "" ||
		b.LastValidBlockHeight <= 0 || b.SimulationSlot <= 0 {
		return fmt.Errorf("incomplete simulated signed transaction")
	}
	wireHash := sha256.Sum256(b.SignedWire)
	if b.SignedWireSHA256 != hex.EncodeToString(wireHash[:]) || len(b.MessageSHA256) != 64 {
		return fmt.Errorf("transaction hash mismatch")
	}
	signature, message, recentBlockhash, signer, err := decodeExactWire(b.SignedWire)
	if err != nil || !ed25519.Verify(signer[:], message, signature) ||
		b.TransactionSignature != encodeBase58(signature) || b.RecentBlockhash != encodeBase58(recentBlockhash[:]) {
		return fmt.Errorf("signed transaction wire does not match persisted evidence")
	}
	if expectedDelegate != (publicKey{}) && signer != expectedDelegate {
		return fmt.Errorf("signed transaction does not use the pinned delegated executor")
	}
	messageHash := sha256.Sum256(message)
	if b.MessageSHA256 != hex.EncodeToString(messageHash[:]) {
		return fmt.Errorf("message hash does not match signed wire")
	}
	return nil
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

func decodeShortVec(data []byte, offset *int) (int, error) {
	value, shift := 0, 0
	for count := 0; count < 5; count++ {
		if *offset >= len(data) {
			return 0, fmt.Errorf("truncated shortvec")
		}
		byteValue := data[*offset]
		*offset++
		value |= int(byteValue&0x7f) << shift
		if byteValue&0x80 == 0 {
			return value, nil
		}
		shift += 7
	}
	return 0, fmt.Errorf("shortvec overflow")
}
