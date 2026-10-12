package backyard

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// v1Read is a compiled message as the cluster decodes it: its programs in
// order and its compute budget.
func v1Read(t *testing.T, message []byte) ([]string, solana.TransactionConfig) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(signedWire(message, make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if tx.Message.GetVersion() != solana.MessageVersionV1 || len(tx.Message.AddressTableLookups) != 0 {
		t.Fatal("not a v1 message")
	}
	var programs []string
	for _, ix := range tx.Message.Instructions {
		programs = append(programs, tx.Message.AccountKeys[ix.ProgramIDIndex].String())
	}
	return programs, tx.Message.TransactionConfig
}

func assertBudget(t *testing.T, name string, config solana.TransactionConfig, units, heap uint32) {
	t.Helper()
	if config.ComputeUnitLimit == nil || *config.ComputeUnitLimit != units || config.LoadedAccountsDataSizeLimit == nil ||
		*config.LoadedAccountsDataSizeLimit != loadedAccountsDataBytes || config.PriorityFee != nil || (heap == 0) != (config.HeapSize == nil) ||
		(heap != 0 && *config.HeapSize != heap) {
		t.Fatalf("%s budget %+v, want %d units and heap %d", name, config, units, heap)
	}
}

// Every Backyard message is the one v1 shape with its compute budget in its
// config and no ComputeBudget instruction: the legacy meter's units (200,000
// an instruction), and on the AUTO lanes at least an AUTO swap's 300,000 and
// the 64 KiB heap their policies need.
func TestEveryMessageCarriesItsBudgetInItsConfig(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	delegate := mustKey(bridgeDelegate)
	klend, squadsID := kamino.ProgramID.String(), squads.ProgramID.String()
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	auto, err := compileKaminoMessageForDelegate(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	programs, config := v1Read(t, auto)
	if strings.Join(programs, " ") != strings.Join([]string{klend, klend, klend, squadsID}, " ") {
		t.Fatalf("AUTO Kamino leg programs %v", programs)
	}
	assertBudget(t, "AUTO Kamino leg", config, 800_000, autoExecutionHeapBytes)
	if again, err := compileKaminoMessageForDelegate(request, delegate); err != nil || !bytes.Equal(auto, again) {
		t.Fatal("AUTO Kamino compile is not deterministic")
	}
	installed, err := compileKaminoMessageForDelegate(kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit), delegate)
	if err != nil {
		t.Fatal(err)
	}
	_, config = v1Read(t, installed)
	assertBudget(t, "installed Kamino leg", config, 800_000, 0)
	swap, err := compileJupiterMessageForDelegate(autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0), delegate)
	if err != nil {
		t.Fatal(err)
	}
	programs, config = v1Read(t, swap)
	if strings.Join(programs, " ") != squadsID {
		t.Fatalf("AUTO swap programs %v", programs)
	}
	assertBudget(t, "AUTO swap", config, autoSwapComputeUnits, autoExecutionHeapBytes)
	m, initializer := autoInitializerRequestFixture(t)
	message, err := m.compileKaminoInitializationMessage(initializer)
	if err != nil {
		t.Fatal(err)
	}
	_, config = v1Read(t, message)
	assertBudget(t, "AUTO initializer", config, autoSwapComputeUnits, autoExecutionHeapBytes)
	initializer.RouteLane = PhaseOneLaneID
	initializer.Policy = testPolicyAccount(policyKey{lane: PhaseOneLaneID, action: InitializeKaminoObligation})
	if message, err = CompileKaminoInitializationMessage(initializer); err != nil {
		t.Fatal(err)
	}
	_, config = v1Read(t, message)
	assertBudget(t, "installed initializer", config, instructionComputeUnits, 0)
	bridge, err := CompileBridgeMessage(bridgeTestRequest(ReportNAV, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, config = v1Read(t, bridge)
	assertBudget(t, "NAV report", config, instructionComputeUnits, 0)
}

// wireSig and wireMsg are a one-signature v1 wire's signature and message.
func wireSig(wire []byte) []byte { return wire[len(wire)-64:] }
func wireMsg(wire []byte) []byte { return wire[:len(wire)-64] }

// signTestWire assembles the exact one-signature wire a delegate would
// broadcast, keyed by a generated test key — never a production signer.
func signTestWire(t *testing.T, key []byte, message []byte) []byte {
	t.Helper()
	return signedWire(message, ed25519.Sign(key, message))
}

// signedTestBuildResult wraps a compiled message into the exact persisted
// evidence shape BuildResult.validateForDelegate checks, signed by the given
// generated test key.
func signedTestBuildResult(t *testing.T, key ed25519.PrivateKey, message []byte) BuildResult {
	t.Helper()
	wire := signTestWire(t, key, message)
	return BuildResult{
		SignedWire:           wire,
		SignedWireSHA256:     sha256Bytes(wire),
		MessageSHA256:        sha256Bytes(message),
		TransactionSignature: encodeBase58(ed25519.Sign(key, message)),
		RecentBlockhash:      bridgeSettings,
		LastValidBlockHeight: 99,
		SimulationSlot:       42,
	}
}

// The persisted-evidence gate admits the one v1 shape signed by the pinned
// delegate, ending in its Squads execute, and refuses another version, a
// foreign signer and drifted evidence.
func TestPersistedBuildResultValidatesTheOneV1Shape(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	delegateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{33}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(delegateKey.Public().(ed25519.PublicKey))
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	message, err := compileKaminoMessageForDelegate(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	result := signedTestBuildResult(t, delegateKey, message)
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatalf("v1 build result rejected: %v", err)
	}
	outsider := publicKeyFromBytes(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{34}, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	if err := result.validateForDelegate(outsider); err == nil || !strings.Contains(err.Error(), "pinned delegated executor") {
		t.Fatalf("wrong delegate accepted: %v", err)
	}
	refreshOnly, err := compileV1Message(delegate, mustKey(bridgeSettings), kaminoPrimeUSDCRefreshInstructionsForRoute(kaminoLegDeposit, RouteID)[:1], v1Budget{units: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string][]byte{
		"v0 version":      append([]byte{0x80}, message[1:]...),
		"two signers":     append([]byte{message[0], 2}, message[2:]...),
		"trailing byte":   append(append([]byte(nil), message...), 0),
		"no Squads last ": refreshOnly,
	} {
		if err := signedTestBuildResult(t, delegateKey, mutated).validateForDelegate(delegate); err == nil {
			t.Fatalf("%s passed the persisted gate", name)
		}
	}
	drifted := result
	drifted.TransactionSignature = encodeBase58(bytes.Repeat([]byte{7}, ed25519.SignatureSize))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "does not match persisted evidence") {
		t.Fatalf("signature drift accepted: %v", err)
	}
	drifted = result
	drifted.MessageSHA256 = sha256Bytes([]byte("drifted"))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "message hash does not match") {
		t.Fatalf("message hash drift accepted: %v", err)
	}
	drifted = result
	drifted.SimulationSlot = 0
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "incomplete simulated signed transaction") {
		t.Fatalf("unsimulated result accepted: %v", err)
	}
	drifted = result
	drifted.SignedWireSHA256 = sha256Bytes([]byte("drifted"))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "transaction hash mismatch") {
		t.Fatalf("wire hash drift accepted: %v", err)
	}
}
