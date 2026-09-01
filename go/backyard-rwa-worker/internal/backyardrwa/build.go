package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
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
	// These bindings are read from the confirmed immutable adaptor config and
	// Squads Settings before a transaction is built. They are repeated here so
	// a stale observation cannot be silently paired with the hard-coded wire.
	AdaptorConfig        string
	Settings             string
	LastAcceptedSequence uint64
	RecentBlockhash      string
	LastValidBlockHeight int64
}

// SignedBridgeTransaction is an exact legacy transaction.  It is suitable for
// sig-verified RPC simulation, but cannot be persisted as a BuildResult until
// that simulation returns its confirmed slot.
type SignedBridgeTransaction struct {
	message              []byte
	signedWire           []byte
	messageSHA256        string
	signedWireSHA256     string
	transactionSignature string
	recentBlockhash      string
	lastValidBlockHeight int64
}

// BuildResult attaches the only post-signing datum: the slot returned from
// simulating this exact wire.  This prevents callers from accidentally
// persisting an unsigned, re-built, or differently simulated transaction.
func (s SignedBridgeTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
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
	bridgeSquadsProgram    = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"
	bridgeSettings         = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"
	bridgeVault            = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	bridgeDelegate         = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
	bridgeAllocationPolicy = "3pUxmFm7cExQRSwHeuycemsa2rf3zsSXLLXoGp5uYYYz"
	bridgeNAVPolicy        = "AfQM1WgcujthouQDhcMZxXmCzG4EUW1qZPnRodioNXmJ"
	bridgeStagePolicy      = "8Y6YHNFLjcRr4XtP9boU8jXGvA1kv8nonWhBWtM91eoq"
	bridgeWithdrawPolicy   = "6hTaRm3B3M1RBmYxzy2utm4PxjCMsptEZjYcKSHi1xya"
	bridgeVoltrProgram     = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
	bridgeVoltrVault       = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"
	bridgeStrategy         = "9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj"
	bridgeProtocol         = "4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y"
	bridgeAdaptorReceipt   = "AsfkxMdVYjMnr2fdTBMUXhq81hgi2hbENXCy9WhUQF7u"
	bridgeStrategyReceipt  = "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6"
	bridgeIdleAuthority    = "EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb"
	bridgeStrategyAuth     = "8fLTf2ufePttZW3Es1xVoW3ows3WjXcuHQkkBCVvHsdH"
	bridgeUSDC             = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	bridgeLPMint           = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"
	bridgeIdleATA          = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"
	bridgeStrategyATA      = "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M"
	bridgeSquadsATA        = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"
	bridgeTokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	bridgeAdaptorProgram   = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"
)

var (
	voltrDepositDiscriminator      = []byte{246, 82, 57, 226, 131, 222, 253, 249}
	voltrWithdrawDiscriminator     = []byte{31, 45, 162, 5, 193, 217, 134, 188}
	adaptorDepositDiscriminator    = []byte{242, 35, 198, 137, 82, 225, 242, 182}
	adaptorWithdrawDiscriminator   = []byte{183, 18, 70, 156, 148, 109, 161, 34}
	squadsExecuteSyncDiscriminator = []byte{90, 81, 187, 81, 39, 70, 128, 78}
)

const (
	bridgeCapRaw uint64 = 1_000_000_000_000
	bridgeMaxNAV uint64 = 2_000_000_000_000
)

// BuildAndSignBridgeTransaction builds exactly one policy-wrapped bridge
// operation.  The only top-level signer is the pinned delegated executor; the
// Squads vault signer is deliberately only an inner CPI meta, which is how the
// Squads program proves PDA authority to Voltr and the adaptor.
func BuildAndSignBridgeTransaction(request BridgeBuildRequest, executor ed25519.PrivateKey) (SignedBridgeTransaction, error) {
	return buildAndSignBridgeTransactionForDelegate(request, executor, mustKey(bridgeDelegate))
}

// buildAndSignBridgeTransactionForDelegate exists solely so package tests can
// verify Solana wire encoding with deterministic non-production key material.
// Production always calls BuildAndSignBridgeTransaction, which pins the real
// delegated executor above.
func buildAndSignBridgeTransactionForDelegate(request BridgeBuildRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedBridgeTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.LastValidBlockHeight <= 0 {
		return SignedBridgeTransaction{}, fmt.Errorf("invalid bridge signing material")
	}
	if request.AdaptorConfig != bridgeStrategy || request.Settings != bridgeSettings ||
		request.LastAcceptedSequence == ^uint64(0) || request.Report.Sequence != request.LastAcceptedSequence+1 {
		return SignedBridgeTransaction{}, fmt.Errorf("bridge config or report sequence is not bound to the confirmed snapshot")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedBridgeTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return SignedBridgeTransaction{}, fmt.Errorf("invalid confirmed blockhash: %w", err)
	}
	inner, policy, constraintIndex, err := bridgeInstruction(request)
	if err != nil {
		return SignedBridgeTransaction{}, err
	}
	outer, err := wrapSquadsPolicyForDelegate(policy, feePayer, expectedDelegate, constraintIndex, inner)
	if err != nil {
		return SignedBridgeTransaction{}, err
	}
	message, err := compileLegacyMessage(feePayer, blockhash, []compiledInstruction{outer})
	if err != nil {
		return SignedBridgeTransaction{}, err
	}
	signature := ed25519.Sign(executor, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	messageDigest := sha256.Sum256(message)
	wireDigest := sha256.Sum256(wire)
	return SignedBridgeTransaction{
		message: message, signedWire: wire,
		messageSHA256:        hex.EncodeToString(messageDigest[:]),
		signedWireSHA256:     hex.EncodeToString(wireDigest[:]),
		transactionSignature: encodeBase58(signature),
		recentBlockhash:      request.RecentBlockhash,
		lastValidBlockHeight: request.LastValidBlockHeight,
	}, nil
}

func bridgeInstruction(request BridgeBuildRequest) (compiledInstruction, publicKey, byte, error) {
	switch request.Action {
	case VoltrAllocateToSquads:
		if request.AmountRaw == 0 || request.AmountRaw > bridgeCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid allocation amount")
		}
		data, err := voltrStrategyData(voltrDepositDiscriminator, adaptorDepositDiscriminator, request.AmountRaw, request.Report)
		if err != nil {
			return compiledInstruction{}, publicKey{}, 0, err
		}
		return voltrDepositInstruction(data), mustKey(bridgeAllocationPolicy), 0, nil
	case ReportNAV:
		if request.AmountRaw != 0 {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("NAV refresh cannot move capital")
		}
		data, err := voltrStrategyData(voltrDepositDiscriminator, adaptorDepositDiscriminator, 0, request.Report)
		if err != nil {
			return compiledInstruction{}, publicKey{}, 0, err
		}
		return voltrDepositInstruction(data), mustKey(bridgeNAVPolicy), 0, nil
	case VoltrRestoreIdle:
		if request.AmountRaw == 0 || request.AmountRaw > bridgeCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid Voltr restore amount")
		}
		data, err := voltrStrategyData(voltrWithdrawDiscriminator, adaptorWithdrawDiscriminator, request.AmountRaw, request.Report)
		if err != nil {
			return compiledInstruction{}, publicKey{}, 0, err
		}
		return voltrWithdrawInstruction(data), mustKey(bridgeWithdrawPolicy), 0, nil
	case StageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > bridgeCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid staging amount")
		}
		return stageInstruction(request.AmountRaw), mustKey(bridgeStagePolicy), 0, nil
	default:
		return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("action %s has no approved bridge transaction", request.Action)
	}
}

func voltrStrategyData(voltrDiscriminator, adaptorDiscriminator []byte, amount uint64, report BridgeReport) ([]byte, error) {
	encodedReport, err := encodeBridgeReport(report)
	if err != nil {
		return nil, err
	}
	// Anchor option<Vec<u8>>: Some (1), u32 len, bytes.  The adaptor
	// discriminator is Voltr's instruction_discriminator option, not a caller
	// selected program instruction.
	data := append([]byte(nil), voltrDiscriminator...)
	data = appendU64(data, amount)
	data = append(data, 1)
	data = appendU32(data, uint32(len(adaptorDiscriminator)))
	data = append(data, adaptorDiscriminator...)
	data = append(data, 1)
	data = appendU32(data, uint32(len(encodedReport)))
	data = append(data, encodedReport...)
	return data, nil
}

func encodeBridgeReport(report BridgeReport) ([]byte, error) {
	if report.Sequence == 0 || report.ObservedSlot == 0 || report.NAVAfterRaw > bridgeMaxNAV {
		return nil, fmt.Errorf("invalid adaptor report fields")
	}
	digest, err := hex.DecodeString(report.SnapshotDigest)
	if err != nil || len(digest) != 32 || allZero(digest) {
		return nil, fmt.Errorf("invalid adaptor snapshot digest")
	}
	data := []byte{1}
	data = appendU64(data, report.Sequence)
	data = appendU64(data, report.ObservedSlot)
	data = appendU64(data, report.NAVAfterRaw)
	return append(data, digest...), nil
}

func voltrDepositInstruction(data []byte) compiledInstruction {
	return compiledInstruction{program: mustKey(bridgeVoltrProgram), data: data, accounts: metas(
		meta(bridgeVault, true, false), meta(bridgeProtocol, false, false), meta(bridgeVoltrVault, false, true), meta(bridgeStrategy, false, true),
		meta(bridgeAdaptorReceipt, false, false), meta(bridgeStrategyReceipt, false, true), meta(bridgeIdleAuthority, false, true), meta(bridgeStrategyAuth, false, true),
		meta(bridgeUSDC, false, true), meta(bridgeLPMint, false, false), meta(bridgeIdleATA, false, true), meta(bridgeStrategyATA, false, true),
		meta(bridgeTokenProgram, false, false), meta(bridgeAdaptorProgram, false, false), meta(bridgeSettings, false, false), meta(bridgeVault, true, false), meta(bridgeSquadsATA, false, true),
	)}
}

func voltrWithdrawInstruction(data []byte) compiledInstruction {
	return compiledInstruction{program: mustKey(bridgeVoltrProgram), data: data, accounts: metas(
		meta(bridgeVault, true, false), meta(bridgeProtocol, false, false), meta(bridgeVoltrVault, false, true), meta(bridgeAdaptorReceipt, false, false),
		meta(bridgeStrategyReceipt, false, true), meta(bridgeStrategy, false, true), meta(bridgeAdaptorProgram, false, false), meta(bridgeIdleAuthority, false, true),
		meta(bridgeStrategyAuth, false, true), meta(bridgeUSDC, false, true), meta(bridgeLPMint, false, false), meta(bridgeIdleATA, false, true), meta(bridgeStrategyATA, false, true),
		meta(bridgeTokenProgram, false, false), meta(bridgeSettings, false, false), meta(bridgeVault, true, false), meta(bridgeSquadsATA, false, true),
	)}
}

func stageInstruction(amount uint64) compiledInstruction {
	data := []byte{12}
	data = appendU64(data, amount)
	data = append(data, 6) // USDC decimals
	return compiledInstruction{program: mustKey(bridgeTokenProgram), data: data, accounts: metas(
		meta(bridgeSquadsATA, false, true), meta(bridgeUSDC, false, false), meta(bridgeStrategyATA, false, true), meta(bridgeVault, true, false),
	)}
}

func wrapSquadsPolicy(policy, executor publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	return wrapSquadsPolicyForDelegate(policy, executor, mustKey(bridgeDelegate), constraintIndex, inner)
}

func wrapSquadsPolicyForDelegate(policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if !isBridgePolicy(policy) || executor != expectedDelegate {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads bridge policy or delegate")
	}
	transactionAccounts := make([]accountMeta, 0, len(inner.accounts)+1)
	indexes := make([]byte, 0, len(inner.accounts))
	for _, account := range inner.accounts {
		indexes = append(indexes, pushOrMergeMeta(&transactionAccounts, account))
	}
	programIndex := pushOrMergeMeta(&transactionAccounts, accountMeta{key: inner.program})
	for index := range transactionAccounts {
		transactionAccounts[index].signer = false
	}
	compiled := []byte{1, programIndex, byte(len(indexes))}
	compiled = append(compiled, indexes...)
	if len(inner.data) > math.MaxUint16 {
		return compiledInstruction{}, fmt.Errorf("bridge instruction data overflows Squads compact payload")
	}
	compiled = appendU16(compiled, uint16(len(inner.data)))
	compiled = append(compiled, inner.data...)
	// Exact borsh layout of Squads execute_transaction_sync_v2,
	// SyncPayload::Policy::ProgramInteraction::SyncTransaction.
	data := append([]byte(nil), squadsExecuteSyncDiscriminator...)
	data = append(data, 0, 1, 1, 1, 1) // vault, signer count, policy, program-interaction, Some(indexes)
	data = appendU32(data, 1)
	data = append(data, constraintIndex) // one exact constraint from the split policy
	data = append(data, 1, 0)            // SyncTransaction, inner vault index
	data = appendU32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	accounts := []accountMeta{{key: policy, writable: true}, {key: mustKey(bridgeSquadsProgram)}, {key: executor, signer: true}}
	accounts = append(accounts, transactionAccounts...)
	return compiledInstruction{program: mustKey(bridgeSquadsProgram), accounts: accounts, data: data}, nil
}

func compileLegacyMessage(feePayer, blockhash publicKey, instructions []compiledInstruction) ([]byte, error) {
	if len(instructions) != 1 {
		return nil, fmt.Errorf("bridge transaction must contain exactly one Squads instruction")
	}
	accounts := []accountMeta{{key: feePayer, signer: true, writable: true}}
	for _, instruction := range instructions {
		for _, account := range instruction.accounts {
			pushOrMergeMeta(&accounts, account)
		}
		pushOrMergeMeta(&accounts, accountMeta{key: instruction.program})
	}
	// Solana's legacy header requires this canonical role ordering.
	sort.SliceStable(accounts[1:], func(i, j int) bool { return accountRank(accounts[i+1]) < accountRank(accounts[j+1]) })
	if accounts[0].key != feePayer || !accounts[0].signer || !accounts[0].writable {
		return nil, fmt.Errorf("fee payer lost canonical position")
	}
	if len(accounts) > math.MaxUint8 {
		return nil, fmt.Errorf("legacy bridge transaction has too many accounts")
	}
	index := make(map[publicKey]byte, len(accounts))
	var required, readonlySigned, readonlyUnsigned byte
	for i, account := range accounts {
		index[account.key] = byte(i)
		if account.signer {
			required++
			if !account.writable {
				readonlySigned++
			}
		} else if !account.writable {
			readonlyUnsigned++
		}
	}
	message := []byte{required, readonlySigned, readonlyUnsigned}
	message = append(message, encodeShortVec(len(accounts))...)
	for _, account := range accounts {
		message = append(message, account.key[:]...)
	}
	message = append(message, blockhash[:]...)
	message = append(message, encodeShortVec(len(instructions))...)
	for _, instruction := range instructions {
		program, ok := index[instruction.program]
		if !ok {
			return nil, fmt.Errorf("missing program account")
		}
		message = append(message, program, byte(len(instruction.accounts)))
		for _, account := range instruction.accounts {
			idx, ok := index[account.key]
			if !ok {
				return nil, fmt.Errorf("missing instruction account")
			}
			message = append(message, idx)
		}
		message = append(message, encodeShortVec(len(instruction.data))...)
		message = append(message, instruction.data...)
	}
	return message, nil
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
func metas(values ...accountMeta) []accountMeta { return values }
func meta(value string, signer, writable bool) accountMeta {
	return accountMeta{key: mustKey(value), signer: signer, writable: writable}
}
func mustKey(value string) publicKey {
	key, err := decodeKey(value)
	if err != nil {
		panic("invalid checked-in bridge identity: " + value)
	}
	return key
}
func publicKeyFromBytes(value []byte) publicKey { var key publicKey; copy(key[:], value); return key }
func appendU16(dst []byte, value uint16) []byte { return append(dst, byte(value), byte(value>>8)) }
func appendU32(dst []byte, value uint32) []byte {
	return append(dst, byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
}
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
	for len(bytes) > 1 && bytes[0] == 0 {
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
	for _, digit := range digits {
		out = append(out, base58Alphabet[digit])
	}
	return string(out)
}
func encodeShortVec(value int) []byte {
	if value < 0 {
		panic("negative shortvec")
	}
	out := []byte{}
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
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
	signature, message, recentBlockhash, signer, err := decodeExactLegacyWire(b.SignedWire)
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

func decodeExactLegacyWire(wire []byte) ([]byte, []byte, publicKey, publicKey, error) {
	offset := 0
	signatureCount, err := decodeShortVec(wire, &offset)
	if err != nil || signatureCount != 1 || len(wire)-offset < ed25519.SignatureSize+3 {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid legacy transaction signature section")
	}
	signature := append([]byte(nil), wire[offset:offset+ed25519.SignatureSize]...)
	offset += ed25519.SignatureSize
	message := wire[offset:]
	messageOffset := 0
	if len(message) < 3 || message[0] != 1 || message[1] != 0 { // one writable outer policy signer
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid legacy transaction header")
	}
	messageOffset = 3
	accountCount, err := decodeShortVec(message, &messageOffset)
	if err != nil || accountCount == 0 || len(message)-messageOffset < accountCount*32+32 {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid legacy account keys")
	}
	keys := make([]publicKey, accountCount)
	for index := range keys {
		copy(keys[index][:], message[messageOffset+index*32:messageOffset+(index+1)*32])
	}
	signer := keys[0]
	messageOffset += accountCount * 32
	var recentBlockhash publicKey
	copy(recentBlockhash[:], message[messageOffset:messageOffset+32])
	messageOffset += 32
	instructionCount, err := decodeShortVec(message, &messageOffset)
	if err != nil || instructionCount != 1 {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy bridge transaction must contain one instruction")
	}
	if messageOffset+2 > len(message) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("truncated legacy instruction")
	}
	programIndex := int(message[messageOffset])
	messageOffset++
	accountIndexCount := int(message[messageOffset])
	messageOffset++
	if accountIndexCount == 0 || messageOffset+accountIndexCount > len(message) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid legacy instruction accounts")
	}
	accountIndexes := append([]byte(nil), message[messageOffset:messageOffset+accountIndexCount]...)
	messageOffset += accountIndexCount
	dataLength, err := decodeShortVec(message, &messageOffset)
	if err != nil || dataLength < 0 || messageOffset+dataLength != len(message) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid legacy instruction data")
	}
	data := message[messageOffset : messageOffset+dataLength]
	if programIndex >= len(keys) || keys[programIndex] != mustKey(bridgeSquadsProgram) ||
		len(accountIndexes) < 3 || int(accountIndexes[0]) >= len(keys) || int(accountIndexes[1]) >= len(keys) || int(accountIndexes[2]) >= len(keys) ||
		keys[accountIndexes[0]] == (publicKey{}) ||
		keys[accountIndexes[1]] != mustKey(bridgeSquadsProgram) || keys[accountIndexes[2]] != signer ||
		len(data) < len(squadsExecuteSyncDiscriminator) || !bytes.Equal(data[:len(squadsExecuteSyncDiscriminator)], squadsExecuteSyncDiscriminator) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy transaction is not an exact Squads policy envelope")
	}
	return signature, message, recentBlockhash, signer, nil
}

func isBridgePolicy(policy publicKey) bool {
	return policy == mustKey(bridgeAllocationPolicy) ||
		policy == mustKey(bridgeNAVPolicy) ||
		policy == mustKey(bridgeStagePolicy) ||
		policy == mustKey(bridgeWithdrawPolicy)
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
