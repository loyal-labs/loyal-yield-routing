package backyard

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"

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
	// These bindings are read from the confirmed immutable adaptor config and
	// Squads Settings before a transaction is built. They are repeated here so
	// a stale observation cannot be silently paired with the hard-coded wire.
	AdaptorConfig        string
	Settings             string
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
	bridgeSettings       = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"
	bridgeSettingsSigner = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"
	bridgeVault          = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	bridgeDelegate       = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
	// Fresh policy seed rollover: candidate public identities at Squads seeds
	// 152-155. legacyPolicyGate asserts only the retired 62-65 set absent;
	// root retires 145-148 operationally before opening.
	bridgeAllocationPolicy = "Bt2SEmvnWFyqSV83CieMHzBjYSL7CJL8fXmTshD2RNAv" // seed 152, VOLTR_ALLOCATE_TO_SQUADS
	bridgeNAVPolicy        = "5r4gVPentTwudZXQAtvjqx8iWfmBjLjnJGBypB7aPi8f" // seed 153, REPORT_NAV
	bridgeStagePolicy      = "7EW76UaxsNTnLG931HTNSteRjhR3s9rcKVJ7UtqyN6e3" // seed 154, STAGE_SQUADS_TO_VOLTR
	bridgeWithdrawPolicy   = "GSY3mcsWHPv7LvH38eZZR6WTj4YiMqdQ9Ai1WRKnR76K" // seed 155, VOLTR_RESTORE_IDLE
	bridgeVoltrVault       = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"
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

// positionLegCapRaw bounds every Kamino and Jupiter leg to 200,000 whole
// tokens of the mint it moves, the same way strategyTwoBridgeLegCapRaw bounds
// the bridge legs. The Squads policies those legs run under (141-144, 149-151,
// 156) embed no spending limit; only bridge policies 152-155 carry one (daily
// USDC). Mint decimals are fixed on chain: USDC, PYUSD, USDS, PRIME, syrupUSDC
// and AUTO have 6, ONyc and USDe have 9 (mint accounts recorded in
// docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json).
var positionLegCapRaw = map[string]uint64{
	bridgeUSDC: 200_000_000_000,
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": 200_000_000_000,     // PYUSD
	"USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA":  200_000_000_000,     // USDS
	"3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7": 200_000_000_000,     // PRIME
	"AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj": 200_000_000_000,     // syrupUSDC
	"GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm": 200_000_000_000,     // AUTO
	"5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5": 200_000_000_000_000, // ONyc
	"DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT": 200_000_000_000_000, // USDe
}

// checkPositionLegCap refuses a leg moving more than its mint's cap, or a mint
// without one.
func checkPositionLegCap(mint string, amountRaw uint64) error {
	cap, ok := positionLegCapRaw[mint]
	if !ok || amountRaw > cap {
		return &BudgetHold{Reason: "position_leg_cap_exceeded", Details: map[string]string{"mint": mint, "amountRaw": strconv.FormatUint(amountRaw, 10)}}
	}
	return nil
}

// BuildAndSignBridgeTransaction builds exactly one policy-wrapped bridge
// operation. Capital/NAV payloads contain the atomic ArmReport -> Voltr pair;
// staging remains one SPL instruction. The only top-level signer is the pinned
// delegated executor and the Squads vault is signer only for inner execution.
func BuildAndSignBridgeTransaction(request BridgeBuildRequest, executor ed25519.PrivateKey) (SignedBridgeTransaction, error) {
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
	inner, policy, indexes, err := ticketedBridgeInstructions(request)
	if err != nil {
		return nil, err
	}
	outer, err := wrapSquadsPolicyForDelegate(policy, delegate, delegate, indexes, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileLegacyMessage(delegate, blockhash, []compiledInstruction{outer})
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func checkedUnsignedMessage(message []byte) ([]byte, error) {
	legacy := len(message) >= 3 && message[0] == 1
	v0 := len(message) >= 4 && message[0] == 0x80 && message[1] == 1
	if (!legacy && !v0) || 1+ed25519.SignatureSize+len(message) > solanaPacketBytes {
		return nil, fmt.Errorf("unsigned message does not fit the single-signer packet envelope")
	}
	return message, nil
}

// buildAndSignBridgeTransactionForDelegate exists solely so package tests can
// verify Solana wire encoding with deterministic non-production key material.
// Production always calls BuildAndSignBridgeTransaction, which pins the real
// delegated executor above.
func buildAndSignBridgeTransactionForDelegate(request BridgeBuildRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedBridgeTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.LastValidBlockHeight <= 0 {
		return SignedBridgeTransaction{}, fmt.Errorf("invalid bridge signing material")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedBridgeTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	message, err := compileBridgeMessageForDelegate(request, expectedDelegate)
	if err != nil {
		return SignedBridgeTransaction{}, err
	}
	signature := ed25519.Sign(executor, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	if len(wire) > solanaPacketBytes {
		return SignedBridgeTransaction{}, fmt.Errorf("bridge packet is %d bytes, exceeds %d", len(wire), solanaPacketBytes)
	}
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
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid allocation amount")
		}
		ix, err := voltrStrategyInstruction(voltr.DepositStrategy, adaptorDepositDiscriminator, request.AmountRaw, request.Report)
		return ix, mustKey(bridgeAllocationPolicy), 0, err
	case ReportNAV:
		if request.AmountRaw != 0 {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("NAV refresh cannot move capital")
		}
		ix, err := voltrStrategyInstruction(voltr.DepositStrategy, adaptorDepositDiscriminator, 0, request.Report)
		return ix, mustKey(bridgeNAVPolicy), 0, err
	case VoltrRestoreIdle:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid Voltr restore amount")
		}
		ix, err := voltrStrategyInstruction(voltr.WithdrawStrategy, adaptorWithdrawDiscriminator, request.AmountRaw, request.Report)
		return ix, mustKey(bridgeWithdrawPolicy), 0, err
	case StageSquadsToVoltr:
		if request.AmountRaw == 0 || request.AmountRaw > strategyTwoBridgeLegCapRaw {
			return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("invalid staging amount")
		}
		return stageInstruction(request.AmountRaw), mustKey(bridgeStagePolicy), 0, nil
	default:
		return compiledInstruction{}, publicKey{}, 0, fmt.Errorf("action %s has no approved bridge transaction", request.Action)
	}
}

// voltrStrategyInstruction is a bridge Voltr deposit_strategy or
// withdraw_strategy: Voltr calls the pinned adaptor instruction with the
// encoded report, and the adaptor's remaining accounts are the Squads
// settings, the vault signer and its USDC custody.
func voltrStrategyInstruction(build func(voltr.StrategyAccounts, uint64, []byte, []byte, ...*solana.AccountMeta) *solana.GenericInstruction,
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
	return sdkInstruction(build(accounts, amount, adaptorDiscriminator, encodedReport,
		solana.Meta(solanaKey(bridgeSettings)), solana.Meta(solanaKey(bridgeVault)).SIGNER(), solana.Meta(solanaKey(bridgeSquadsATA)).WRITE())), nil
}

func encodeBridgeReport(report BridgeReport) ([]byte, error) {
	if report.Sequence == 0 || report.Sequence != report.ObservedSlot || report.NAVAfterRaw > bridgeMaxNAV {
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

func stageInstruction(amount uint64) compiledInstruction {
	return sdkInstruction(spl.TransferChecked(solana.TokenProgramID, solanaKey(bridgeSquadsATA), solanaKey(bridgeUSDC), solanaKey(bridgeStrategyATA), solanaKey(bridgeVault), amount, 6))
}

func solanaKey(value string) solana.PublicKey { return solana.PublicKey(mustKey(value)) }

// sdkInstruction is an instruction solana-go built.
func sdkInstruction(ix *solana.GenericInstruction) compiledInstruction {
	out := compiledInstruction{program: publicKey(ix.ProgID), data: ix.DataBytes}
	for _, account := range ix.AccountValues {
		out.accounts = append(out.accounts, accountMeta{key: publicKey(account.PublicKey), signer: account.IsSigner, writable: account.IsWritable})
	}
	return out
}

func wrapSquadsPolicyForDelegate(policy, executor, expectedDelegate publicKey, constraintIndexes []byte, inner []compiledInstruction) (compiledInstruction, error) {
	if !isBridgePolicy(policy) || executor != expectedDelegate {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads bridge policy or delegate")
	}
	return wrapSquadsPolicy(policy, executor, constraintIndexes, inner...)
}

// wrapSquadsPolicy runs inner as the bridge vault (smart account index 0)
// through policy, signed by executor.
func wrapSquadsPolicy(policy, executor publicKey, constraintIndexes []byte, inner ...compiledInstruction) (compiledInstruction, error) {
	execute := squads.ExecuteSync{Policy: solana.PublicKey(policy), Signer: solana.PublicKey(executor), ConstraintIndexes: constraintIndexes}
	for _, ix := range inner {
		instruction := squads.Instruction{ProgramID: solana.PublicKey(ix.program), Data: ix.data}
		for _, account := range ix.accounts {
			instruction.Accounts = append(instruction.Accounts, solana.AccountMeta{PublicKey: solana.PublicKey(account.key), IsSigner: account.signer, IsWritable: account.writable})
		}
		execute.Inner = append(execute.Inner, instruction)
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

func compileLegacyMessage(feePayer, blockhash publicKey, instructions []compiledInstruction) ([]byte, error) {
	if len(instructions) != 1 {
		return nil, fmt.Errorf("bridge transaction must contain exactly one Squads instruction")
	}
	return encodeLegacyMessage(feePayer, blockhash, instructions)
}

// Encoding is separate from each caller's closed instruction-set validation.
// The bridge/signing boundary above still permits exactly one instruction, and
// the installed Kamino compiler keeps its own exact-four gate with its own
// serializer. The AUTO resource wrappers encode their canonical heap frame plus
// one already-validated payload — two instructions, inside this bound.
// Optional capture keys ride along as read-only non-signing static accounts so
// one unsigned simulation can return a full observed batch; merging through the
// same table means an instruction's required privileges are never lowered.
func encodeLegacyMessage(feePayer, blockhash publicKey, instructions []compiledInstruction, capture ...publicKey) ([]byte, error) {
	if len(instructions) == 0 || len(instructions) > 4 {
		return nil, fmt.Errorf("unsupported legacy instruction count")
	}
	accounts := []accountMeta{{key: feePayer, signer: true, writable: true}}
	for _, key := range capture {
		pushOrMergeMeta(&accounts, accountMeta{key: key})
	}
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
	// Route by the encoded message version byte after the single-signature
	// section: legacy header 1, versioned header 0x80. Each decoder enforces
	// its own signature section and header, so any other version — or a
	// misrouted count — fails closed in the decoder it reaches.
	var signature, message []byte
	var recentBlockhash, signer publicKey
	var err error
	if len(b.SignedWire) >= 66 && b.SignedWire[65] == 0x80 {
		signature, message, recentBlockhash, signer, err = decodeExactV0Wire(b.SignedWire)
	} else {
		signature, message, recentBlockhash, signer, err = decodeExactLegacyWire(b.SignedWire)
	}
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
	if err != nil || (instructionCount != 1 && instructionCount != 2 && instructionCount != 3 &&
		instructionCount != 4 && instructionCount != 5) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy transaction has an unsupported instruction count")
	}
	instructions := make([]decodedLegacyInstruction, instructionCount)
	for index := range instructions {
		instruction, nextOffset, err := decodeLegacyInstruction(message, messageOffset, keys)
		if err != nil {
			return nil, nil, publicKey{}, publicKey{}, err
		}
		instructions[index] = instruction
		messageOffset = nextOffset
	}
	if messageOffset != len(message) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("trailing legacy transaction bytes")
	}
	outer := instructions[len(instructions)-1]
	if outer.program != publicKey(squads.ProgramID) || len(outer.accountIndexes) < 3 ||
		keys[outer.accountIndexes[0]] == (publicKey{}) ||
		keys[outer.accountIndexes[1]] != publicKey(squads.ProgramID) || keys[outer.accountIndexes[2]] != signer ||
		!bytes.HasPrefix(outer.data, squads.ExecuteTransactionSyncV2Discriminator[:]) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy transaction is not an exact Squads policy envelope")
	}
	if instructionCount == 4 && !isExactKaminoTransaction(instructions) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy Kamino transaction has an invalid refresh or embedded leg")
	}
	// The AUTO resource envelopes are the only other admitted shapes: either
	// the legacy heap-only persisted envelope — the canonical reviewed
	// ComputeBudget heap frame leads, alone — or the AUTO swap envelope, the
	// heap frame plus the canonical compute-unit frame leading the payload. In
	// both cases the payload behind the resource prefix validates against the
	// exact existing sequence gates. An arbitrary, duplicated or reordered
	// compute instruction fails closed.
	if (instructionCount == 2 || instructionCount == 5) && !isAutoResourceHeapTransaction(instructions) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy AUTO transaction does not carry the exact reviewed heap frame first")
	}
	if instructionCount == 3 && !isAutoSwapResourceTransaction(instructions) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy AUTO swap transaction does not carry the exact reviewed heap and compute-unit frames first")
	}
	if instructionCount == 5 && !isExactAutoKaminoTransaction(instructions[1:]) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("legacy AUTO Kamino transaction has an invalid refresh or embedded leg")
	}
	return signature, message, recentBlockhash, signer, nil
}

// decodeExactV0Wire is the narrow versioned counterpart of
// decodeExactLegacyWire: one writable required signer and either the installed
// single Squads execute, the legacy heap-only AUTO envelope, or the AUTO swap
// envelope with the canonical heap and compute-unit frames ahead of it.
// Invoked programs must stay static exactly as the compiler emits them, the Squads authority pins must be static keys, and
// lookup-resolved venue accounts only need to stay inside the loaded index
// space.
func decodeExactV0Wire(wire []byte) ([]byte, []byte, publicKey, publicKey, error) {
	offset := 0
	signatureCount, err := decodeShortVec(wire, &offset)
	if err != nil || signatureCount != 1 || len(wire)-offset < ed25519.SignatureSize+4 {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid versioned transaction signature section")
	}
	signature := append([]byte(nil), wire[offset:offset+ed25519.SignatureSize]...)
	offset += ed25519.SignatureSize
	message := wire[offset:]
	if len(message) < 4 || message[0] != 0x80 || message[1] != 1 || message[2] != 0 { // one writable outer policy signer
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid versioned transaction header")
	}
	messageOffset := 4
	staticCount, err := decodeShortVec(message, &messageOffset)
	if err != nil || staticCount == 0 || len(message)-messageOffset < staticCount*32+32 {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid versioned static account keys")
	}
	keys := make([]publicKey, staticCount)
	for index := range keys {
		copy(keys[index][:], message[messageOffset+index*32:messageOffset+(index+1)*32])
	}
	signer := keys[0]
	messageOffset += staticCount * 32
	var recentBlockhash publicKey
	copy(recentBlockhash[:], message[messageOffset:messageOffset+32])
	messageOffset += 32
	instructionCount, err := decodeShortVec(message, &messageOffset)
	if err != nil || (instructionCount != 1 && instructionCount != 2 && instructionCount != 3) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned transaction has an unsupported instruction count")
	}
	type versionedInstruction struct {
		program        publicKey
		accountIndexes []byte
		data           []byte
	}
	instructions := make([]versionedInstruction, instructionCount)
	for index := range instructions {
		if messageOffset+2 > len(message) {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("truncated versioned instruction")
		}
		programIndex := int(message[messageOffset])
		messageOffset++
		if programIndex >= staticCount {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned instruction program is not static")
		}
		accountCount, err := decodeShortVec(message, &messageOffset)
		if err != nil {
			return nil, nil, publicKey{}, publicKey{}, err
		}
		if messageOffset+accountCount > len(message) {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("truncated versioned instruction accounts")
		}
		accountIndexes := append([]byte(nil), message[messageOffset:messageOffset+accountCount]...)
		messageOffset += accountCount
		dataLength, err := decodeShortVec(message, &messageOffset)
		if err != nil || messageOffset+dataLength > len(message) {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("invalid versioned instruction data")
		}
		instructions[index] = versionedInstruction{program: keys[programIndex], accountIndexes: accountIndexes,
			data: append([]byte(nil), message[messageOffset:messageOffset+dataLength]...)}
		messageOffset += dataLength
	}
	loaded := 0
	lookupCount, err := decodeShortVec(message, &messageOffset)
	if err != nil {
		return nil, nil, publicKey{}, publicKey{}, err
	}
	for index := 0; index < lookupCount; index++ {
		if messageOffset+32 > len(message) {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("truncated versioned lookup table key")
		}
		messageOffset += 32
		for _, width := range []string{"writable", "readonly"} {
			count, err := decodeShortVec(message, &messageOffset)
			if err != nil || messageOffset+count > len(message) {
				return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("truncated versioned %s lookup indexes", width)
			}
			messageOffset += count
			loaded += count
		}
	}
	if messageOffset != len(message) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("trailing versioned transaction bytes")
	}
	indexSpace := staticCount + loaded
	outer := instructions[len(instructions)-1]
	squadsKey := publicKey(squads.ProgramID)
	if outer.program != squadsKey || len(outer.accountIndexes) < 3 ||
		int(outer.accountIndexes[0]) >= staticCount || keys[outer.accountIndexes[0]] == (publicKey{}) ||
		int(outer.accountIndexes[1]) >= staticCount || keys[outer.accountIndexes[1]] != squadsKey ||
		int(outer.accountIndexes[2]) >= staticCount || keys[outer.accountIndexes[2]] != signer ||
		!bytes.HasPrefix(outer.data, squads.ExecuteTransactionSyncV2Discriminator[:]) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned transaction is not an exact Squads policy envelope")
	}
	for _, instruction := range instructions {
		for _, accountIndex := range instruction.accountIndexes {
			if int(accountIndex) >= indexSpace {
				return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned instruction account index outside the loaded space")
			}
		}
	}
	if instructionCount == 2 {
		heap := autoComputeBudgetHeapInstruction()
		first := instructions[0]
		if first.program != heap.program || len(first.accountIndexes) != 0 || !bytesEqual(first.data, heap.data) {
			return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned AUTO transaction does not carry the exact reviewed heap frame first")
		}
	}
	// The three-instruction AUTO swap envelope carries the exact heap frame and
	// the exact compute-unit frame ahead of the already-pinned Squads outer, so
	// no extra or reordered budget frame fits. Six-instruction AUTO Kamino
	// messages stay unadmitted on this path.
	if instructionCount == 3 &&
		(!isCanonicalHeapFrame(instructions[0].program, len(instructions[0].accountIndexes), instructions[0].data) ||
			!isCanonicalUnitLimitFrame(instructions[1].program, len(instructions[1].accountIndexes), instructions[1].data)) {
		return nil, nil, publicKey{}, publicKey{}, fmt.Errorf("versioned AUTO swap transaction does not carry the exact reviewed heap and compute-unit frames first")
	}
	return signature, message, recentBlockhash, signer, nil
}

type decodedLegacyInstruction struct {
	program        publicKey
	accountIndexes []byte
	accounts       []publicKey
	data           []byte
}

func decodeLegacyInstruction(message []byte, offset int, keys []publicKey) (decodedLegacyInstruction, int, error) {
	if offset+2 > len(message) {
		return decodedLegacyInstruction{}, offset, fmt.Errorf("truncated legacy instruction")
	}
	programIndex := int(message[offset])
	offset++
	accountIndexCount := int(message[offset])
	offset++
	// A zero account list is admissible here only so the canonical AUTO heap
	// frame can decode; every sequence gate below refuses a zero-account
	// payload instruction (the Squads outer needs its three authority pins and
	// the refresh comparisons match on account lists).
	if programIndex >= len(keys) || offset+accountIndexCount > len(message) {
		return decodedLegacyInstruction{}, offset, fmt.Errorf("invalid legacy instruction accounts")
	}
	accountIndexes := append([]byte(nil), message[offset:offset+accountIndexCount]...)
	offset += accountIndexCount
	for _, accountIndex := range accountIndexes {
		if int(accountIndex) >= len(keys) {
			return decodedLegacyInstruction{}, offset, fmt.Errorf("invalid legacy instruction account index")
		}
	}
	accounts := make([]publicKey, len(accountIndexes))
	for index, accountIndex := range accountIndexes {
		accounts[index] = keys[accountIndex]
	}
	dataLength, err := decodeShortVec(message, &offset)
	if err != nil || dataLength < 0 || offset+dataLength > len(message) {
		return decodedLegacyInstruction{}, offset, fmt.Errorf("invalid legacy instruction data")
	}
	data := append([]byte(nil), message[offset:offset+dataLength]...)
	offset += dataLength
	return decodedLegacyInstruction{program: keys[programIndex], accountIndexes: accountIndexes, accounts: accounts, data: data}, offset, nil
}

func isExactKaminoTransaction(instructions []decodedLegacyInstruction) bool {
	return isExactKaminoTransactionForLanes(instructions,
		[]string{RouteID, PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"})
}

// isExactAutoKaminoTransaction is the AUTO variant of the exact Kamino gate:
// the identical refresh-plus-policy matching restricted to the candidate AUTO
// route, applied only to the payload behind the canonical heap frame. A
// stripped AUTO wire therefore still fails the installed four-instruction
// gate, whose lane list never included the candidate route.
func isExactAutoKaminoTransaction(instructions []decodedLegacyInstruction) bool {
	return isExactKaminoTransactionForLanes(instructions, []string{autoAUTOPYUSD.Lane})
}

func isExactKaminoTransactionForLanes(instructions []decodedLegacyInstruction, lanes []string) bool {
	if len(instructions) != 4 {
		return false
	}
	for _, lane := range lanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			continue
		}
		for _, leg := range []kaminoPrimeUSDCLeg{kaminoLegDeposit, kaminoLegBorrow, kaminoLegRepay, kaminoLegWithdraw} {
			var topologies [][]string
			switch leg {
			case kaminoLegDeposit:
				// Initial deposit (flat obligation) and leveraged redeposit
				// (both reserves); AUTO and OnRe also admit the plan B3 top-up
				// deposit into a debt-free obligation (collateral only).
				topologies = [][]string{{}, {route.Kamino.CollateralReserve, route.Kamino.DebtReserve}}
				if lane == autoAUTOPYUSD.Lane || lane == "OnRe/ONyc/USDC" {
					topologies = append(topologies, []string{route.Kamino.CollateralReserve})
				}
			case kaminoLegBorrow:
				topologies = [][]string{{route.Kamino.CollateralReserve}}
				// B2 leverage_up 1.5x -> 1.75x borrows beside existing debt
				// (AUTO and OnRe only).
				if lane == autoAUTOPYUSD.Lane || lane == "OnRe/ONyc/USDC" {
					topologies = append(topologies, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve})
				}
			case kaminoLegWithdraw:
				topologies = [][]string{
					{route.Kamino.CollateralReserve},
					{route.Kamino.CollateralReserve, route.Kamino.DebtReserve},
				}
			case kaminoLegRepay:
				topologies = [][]string{{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}}
			}
			for _, topology := range topologies {
				expected := kaminoPrimeUSDCRefreshInstructionsForRequest(leg, KaminoPrimeUSDCRequest{RouteLane: lane, ObligationReserves: topology})
				matches := len(expected) == len(instructions)-1
				for index := range expected {
					if !matches || instructions[index].program != expected[index].program ||
						!bytes.Equal(instructions[index].data, expected[index].data) ||
						len(instructions[index].accounts) != len(expected[index].accounts) {
						matches = false
						break
					}
					for accountIndex, account := range instructions[index].accounts {
						if account != expected[index].accounts[accountIndex].key {
							matches = false
							break
						}
					}
				}
				if matches && isExactKaminoSquadsInnerForRoute(instructions[3], leg, lane) {
					return true
				}
			}
		}
	}
	return false
}

func isExactKaminoSquadsInnerForRoute(outer decodedLegacyInstruction, leg kaminoPrimeUSDCLeg, lane string) bool {
	// Exact Borsh envelope emitted by wrapSquadsKaminoPolicy:
	// discriminator | vault | signer count | policy kind | interaction kind |
	// Some(constraint indexes) | vec len | index | sync tx | inner vault |
	// compact payload len | compact payload.
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return false
	}
	if len(outer.accounts) < 4 || len(outer.data) < 27 ||
		!bytes.Equal(outer.data[:8], squads.ExecuteTransactionSyncV2Discriminator[:]) ||
		!bytes.Equal(outer.data[8:13], []byte{0, 1, 1, 1, 1}) ||
		readU32LE(outer.data[13:17]) != 1 || outer.data[17] != kaminoConstraintIndexForRoute(route, leg) ||
		!bytes.Equal(outer.data[18:20], []byte{1, 0}) {
		return false
	}
	compactLength := int(readU32LE(outer.data[20:24]))
	if compactLength != len(outer.data)-24 {
		return false
	}
	compact := outer.data[24:]
	if len(compact) < 5 || compact[0] != 1 {
		return false
	}
	transactionAccounts := outer.accounts[3:]
	programIndex := int(compact[1])
	accountCount := int(compact[2])
	if programIndex >= len(transactionAccounts) || transactionAccounts[programIndex] != publicKey(kamino.ProgramID) ||
		accountCount == 0 || 3+accountCount+2 > len(compact) {
		return false
	}
	wantAccounts := kaminoLegMetasForRoute(leg, lane)
	if len(wantAccounts) != accountCount {
		return false
	}
	expectedTransactionAccounts := make([]accountMeta, 0, len(wantAccounts)+1)
	for _, account := range wantAccounts {
		pushOrMergeMeta(&expectedTransactionAccounts, account)
	}
	pushOrMergeMeta(&expectedTransactionAccounts, accountMeta{key: publicKey(kamino.ProgramID)})
	if len(transactionAccounts) != len(expectedTransactionAccounts) {
		return false
	}
	for index, account := range transactionAccounts {
		if account != expectedTransactionAccounts[index].key {
			return false
		}
	}
	for index, compactIndex := range compact[3 : 3+accountCount] {
		if int(compactIndex) >= len(transactionAccounts) || transactionAccounts[compactIndex] != wantAccounts[index].key {
			return false
		}
	}
	dataLengthOffset := 3 + accountCount
	dataLength := int(compact[dataLengthOffset]) | int(compact[dataLengthOffset+1])<<8
	data := compact[dataLengthOffset+2:]
	return dataLength == len(data) && len(data) == 16 &&
		bytes.Equal(data[:8], kaminoLegDiscriminator(leg)) &&
		readU64(data[8:]) > 0 && readU64(data[8:]) <= bridgeCapRaw
}

func kaminoLegMetasForRoute(leg kaminoPrimeUSDCLeg, lane string) []accountMeta {
	// The basic lanes and the AUTO candidate lane (explicitly not BasicPolicy)
	// use their own route-resolved metas, as the compiler emits them through
	// kaminoPacketForRoute; every other lane keeps the installed PRIME graph.
	route, err := runtimeRoute(lane)
	if lane == RouteID || err != nil || !route.BasicPolicy && route.Lane != autoAUTOPYUSD.Lane {
		route, _ = runtimeRoute(RouteID)
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	switch leg {
	case kaminoLegDeposit:
		return deposit
	case kaminoLegBorrow:
		return borrow
	case kaminoLegRepay:
		return repay
	case kaminoLegWithdraw:
		return withdraw
	default:
		return nil
	}
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

func readU32LE(value []byte) uint32 {
	return uint32(value[0]) | uint32(value[1])<<8 | uint32(value[2])<<16 | uint32(value[3])<<24
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
