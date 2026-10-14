package backyard

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
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

// A fresh lane's entry over its new, empty obligation, as the looping vault's
// Maple canary met it on mainnet (2026-10-14): the entry swap, then the
// deposit of its output. Each message requests the heap Squads needs to
// execute its policy (the swap overran the default 32 KiB after Jupiter
// returned: ProgramFailedToComplete), and the deposit's RefreshObligation
// names no reserve, since KLend takes exactly the obligation's occupied
// slots (an extra one is InvalidAccountInput, 6006).
func TestFreshObligationEntryCompilesAsTheChainRunsIt(t *testing.T) {
	t.Parallel()
	delegate := mustKey(bridgeDelegate)
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	swap, err := compileJupiterMessageForDelegate(rwa, fixtureSwapRequest(t, SelectedRouteID, SwapStableToCollateralStep), delegate)
	if err != nil {
		t.Fatal(err)
	}
	_, config := v1Read(t, swap)
	assertBudget(t, "Maple entry swap", config, instructionComputeUnits, 64*1024)

	data := make([]byte, kamino.ObligationSize)
	copy(data, kamino.ObligationDiscriminator[:])
	empty := ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kamino.ProgramID.String(), Lamports: 1, Data: data}
	policies, _, _ := stepTestPolicies(t)
	deposit, err := embeddedTestManifest(t).kaminoPacketForRoute(policies, OpenRouteStep, kaminoLegDeposit, 33_507_178, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if deposit.ObligationReserves, err = observedObligationTopology([]ConfirmedAccount{empty}, route); err != nil {
		t.Fatal(err)
	}
	message, err := compileKaminoMessageForDelegate(rwa, deposit, delegate)
	if err != nil {
		t.Fatal(err)
	}
	_, config = v1Read(t, message)
	assertBudget(t, "Maple entry deposit", config, 800_000, 64*1024)
	tx, err := solana.TransactionFromBytes(signedWire(message, make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	for _, ix := range tx.Message.Instructions {
		if bytes.HasPrefix(ix.Data, kamino.RefreshObligationDiscriminator[:]) {
			refreshes++
			if len(ix.Accounts) != 2 {
				t.Fatalf("an empty obligation's refresh names %d accounts, want its market and itself", len(ix.Accounts))
			}
		}
	}
	if refreshes != 1 {
		t.Fatalf("deposit carries %d obligation refreshes", refreshes)
	}
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
