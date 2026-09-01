package backyardrwa

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// KaminoPrimeUSDCAccounts is the exact account list produced by the checked
// Kamino SDK for one leg.  Reserve-owned token accounts are deliberately read
// from the confirmed reserve graph; they are not guessed or derived here.
// The builder accepts no route, mint, or program override.
type KaminoPrimeUSDCAccounts []struct {
	Address  string
	Signer   bool
	Writable bool
}

// KaminoPrimeUSDCRequest describes one policy-wrapped leg of the fixed route.
// OPEN is either its PRIME collateral deposit or USDC borrow leg; DELEVER is
// either its USDC repayment or PRIME collateral withdrawal leg.  A swap is
// intentionally outside this narrow builder and cannot be smuggled in as an
// arbitrary program instruction.
type KaminoPrimeUSDCRequest struct {
	Action                  Action
	AmountRaw               uint64
	Policy                  string
	PolicyConstraintIndex   byte
	PolicyAccountDataSHA256 string
	Accounts                KaminoPrimeUSDCAccounts
	Data                    []byte
	RecentBlockhash         string
	LastValidBlockHeight    int64
}

type SignedKaminoTransaction struct {
	message              []byte
	signedWire           []byte
	messageSHA256        string
	signedWireSHA256     string
	transactionSignature string
	recentBlockhash      string
	lastValidBlockHeight int64
}

const (
	kaminoPrimeUSDCProgram        = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	kaminoPrimeMarket             = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	kaminoPrimeReserve            = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
	kaminoUSDCReserve             = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"
	kaminoPrimeUSDCCollateralMint = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	kaminoInstructions            = "Sysvar1nstructions1111111111111111111111111"
)

var (
	kaminoDepositCollateral  = []byte{129, 199, 4, 2, 222, 39, 26, 46}
	kaminoBorrowUSDC         = []byte{121, 127, 18, 204, 73, 245, 225, 65}
	kaminoRepayUSDC          = []byte{145, 178, 13, 225, 76, 240, 147, 72}
	kaminoWithdrawCollateral = []byte{75, 93, 93, 220, 34, 150, 218, 196}
)

// BuildAndSignKaminoPrimeUSDCTransaction creates the legacy Solana wire for
// one exact Kamino SDK instruction under the supplied confirmed Squads policy.
// It signs but never simulates, persists, or broadcasts the result.
func BuildAndSignKaminoPrimeUSDCTransaction(request KaminoPrimeUSDCRequest, executor ed25519.PrivateKey) (SignedKaminoTransaction, error) {
	return buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, executor, mustKey(bridgeDelegate))
}

func buildAndSignKaminoPrimeUSDCTransactionForDelegate(request KaminoPrimeUSDCRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedKaminoTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.LastValidBlockHeight <= 0 {
		return SignedKaminoTransaction{}, fmt.Errorf("invalid Kamino signing material")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedKaminoTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return SignedKaminoTransaction{}, fmt.Errorf("invalid confirmed blockhash: %w", err)
	}
	inner, err := kaminoPrimeUSDCInstruction(request)
	if err != nil {
		return SignedKaminoTransaction{}, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) || !validSHA256(request.PolicyAccountDataSHA256) {
		return SignedKaminoTransaction{}, fmt.Errorf("Kamino policy is not bound to confirmed catalog bytes")
	}
	outer, err := wrapSquadsKaminoPolicy(policy, feePayer, expectedDelegate, request.PolicyConstraintIndex, inner)
	if err != nil {
		return SignedKaminoTransaction{}, err
	}
	message, err := compileLegacyMessage(feePayer, blockhash, []compiledInstruction{outer})
	if err != nil {
		return SignedKaminoTransaction{}, err
	}
	signature := ed25519.Sign(executor, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	messageDigest := sha256.Sum256(message)
	wireDigest := sha256.Sum256(wire)
	return SignedKaminoTransaction{
		message: message, signedWire: wire,
		messageSHA256: hex.EncodeToString(messageDigest[:]), signedWireSHA256: hex.EncodeToString(wireDigest[:]),
		transactionSignature: encodeBase58(signature), recentBlockhash: request.RecentBlockhash,
		lastValidBlockHeight: request.LastValidBlockHeight,
	}, nil
}

func kaminoPrimeUSDCInstruction(request KaminoPrimeUSDCRequest) (compiledInstruction, error) {
	if request.AmountRaw == 0 || len(request.Accounts) == 0 || len(request.Data) != 16 {
		return compiledInstruction{}, fmt.Errorf("incomplete exact Kamino PRIME/USDC packet")
	}
	if got := readU64(request.Data[8:]); got != request.AmountRaw {
		return compiledInstruction{}, fmt.Errorf("Kamino packet amount does not match decision")
	}
	accounts := make([]accountMeta, len(request.Accounts))
	for i, input := range request.Accounts {
		key, err := decodeKey(input.Address)
		if err != nil {
			return compiledInstruction{}, fmt.Errorf("invalid Kamino account %d", i)
		}
		accounts[i] = accountMeta{key: key, signer: input.Signer, writable: input.Writable}
	}
	if len(accounts) < 7 || accounts[0].key != mustKey(bridgeVault) || !accounts[0].signer || accounts[1].key == (publicKey{}) || accounts[2].key != mustKey(kaminoPrimeMarket) {
		return compiledInstruction{}, fmt.Errorf("Kamino packet is not owned by the fixed Squads PRIME route")
	}
	if !matchesKaminoStep(request.Action, request.Data[:8], accounts) {
		return compiledInstruction{}, fmt.Errorf("Kamino packet is not an approved PRIME/USDC lifecycle step")
	}
	return compiledInstruction{program: mustKey(kaminoPrimeUSDCProgram), accounts: accounts, data: append([]byte(nil), request.Data...)}, nil
}

func matchesKaminoStep(action Action, discriminator []byte, accounts []accountMeta) bool {
	match := func(expected []byte, reserveIndex int, reserve string, minAccounts int) bool {
		return len(accounts) >= minAccounts && string(discriminator) == string(expected) && accounts[reserveIndex].key == mustKey(reserve)
	}
	switch action {
	case OpenPrimeUSDCStep:
		// depositReserveLiquidityAndObligationCollateral or borrowObligationLiquidity
		return match(kaminoDepositCollateral, 4, kaminoPrimeReserve, 14) || match(kaminoBorrowUSDC, 4, kaminoUSDCReserve, 12)
	case DeleverPrimeUSDCStep:
		// repayObligationLiquidity or withdrawObligationCollateralAndRedeemReserveCollateral
		return match(kaminoRepayUSDC, 3, kaminoUSDCReserve, 9) || match(kaminoWithdrawCollateral, 4, kaminoPrimeReserve, 14)
	default:
		return false
	}
}

func wrapSquadsKaminoPolicy(policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != mustKey(kaminoPrimeUSDCProgram) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Kamino policy or delegate")
	}
	transactionAccounts := make([]accountMeta, 0, len(inner.accounts)+1)
	indexes := make([]byte, 0, len(inner.accounts))
	for _, account := range inner.accounts {
		indexes = append(indexes, pushOrMergeMeta(&transactionAccounts, account))
	}
	programIndex := pushOrMergeMeta(&transactionAccounts, accountMeta{key: inner.program})
	for i := range transactionAccounts {
		transactionAccounts[i].signer = false
	}
	compiled := []byte{1, programIndex, byte(len(indexes))}
	compiled = append(compiled, indexes...)
	compiled = appendU16(compiled, uint16(len(inner.data)))
	compiled = append(compiled, inner.data...)
	data := append([]byte(nil), squadsExecuteSyncDiscriminator...)
	data = append(data, 0, 1, 1, 1, 1)
	data = appendU32(data, 1)
	data = append(data, constraintIndex, 1, 0)
	data = appendU32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	accounts := []accountMeta{{key: policy, writable: true}, {key: mustKey(bridgeSquadsProgram)}, {key: executor, signer: true}}
	accounts = append(accounts, transactionAccounts...)
	return compiledInstruction{program: mustKey(bridgeSquadsProgram), accounts: accounts, data: data}, nil
}

func (s SignedKaminoTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 {
		return BuildResult{}, fmt.Errorf("exact signed Kamino transaction was not simulated")
	}
	return BuildResult{MessageSHA256: s.messageSHA256, SignedWire: append([]byte(nil), s.signedWire...), SignedWireSHA256: s.signedWireSHA256, TransactionSignature: s.transactionSignature, RecentBlockhash: s.recentBlockhash, LastValidBlockHeight: s.lastValidBlockHeight, SimulationSlot: simulationSlot}, nil
}

// KaminoExecutionEvidence is intentionally explicit. The observer supplies a
// coherent confirmed account graph and the already-validated expected effects;
// this package only constructs the exact, policy-wrapped, signed wire.
type KaminoExecutionEvidence struct {
	Request         KaminoPrimeUSDCRequest
	ExpectedEffects ExpectedEffects
}

func BuildSimulateAndPersistKamino(ctx context.Context, database *Database, rpc *RPCClient, operationID string, evidence KaminoExecutionEvidence) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("Kamino runtime dependencies are required")
	}
	if _, err := kaminoPrimeUSDCInstruction(evidence.Request); err != nil {
		return err
	}
	effects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(effects); err != nil {
		return err
	}
	signer, err := loadPinnedPolicySigner()
	if err != nil {
		return err
	}
	signed, err := BuildAndSignKaminoPrimeUSDCTransaction(evidence.Request, signer)
	if err != nil {
		return err
	}
	if err := database.MarkBuilt(ctx, operationID, signed.messageSHA256, effects); err != nil {
		return err
	}
	simulation, err := rpc.SimulateSignedTransaction(ctx, signed.signedWire)
	if err != nil {
		return err
	}
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}

func validSHA256(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}
func readU64(value []byte) uint64 {
	var out uint64
	for i := 7; i >= 0; i-- {
		out = out<<8 | uint64(value[i])
	}
	return out
}
