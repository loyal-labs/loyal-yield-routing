package backyardrwa

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestR03PlanRequiresSetupPreludeForAbsentObligation(t *testing.T) {
	wire := base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 64)...))
	plan := R03LifecyclePlan{Lane: SelectedRouteID, ProtectedAddresses: []string{"obligation"}, ObligationAddress: "obligation", ObligationAbsent: true, Transactions: []R03LifecycleTransaction{{Role: "entry", Phase: "entry", PacketBytes: 65, TransactionBase64: wire}, {Role: "unwind", Phase: "unwind", PacketBytes: 65, TransactionBase64: wire}, {Role: "return", Phase: "return", PacketBytes: 65, TransactionBase64: wire}, {Role: "nav", Phase: "nav", PacketBytes: 65, TransactionBase64: wire}}, Hold: R03HoldRecord{Action: "HOLD", Reason: "ready", ObservationID: "obs", Slot: 1}}
	if err := plan.validate(); err == nil || !strings.Contains(err.Error(), "setup-authority init-obligation") {
		t.Fatalf("absent obligation plan was not forced through the explicit prelude: %v", err)
	}
}

func TestR03StateHashAndSignatureAbsenceAreDeterministic(t *testing.T) {
	if !allSignaturesAbsent([]SignatureObservation{{}, {}}) {
		t.Fatal("nil signature statuses were not treated as absent")
	}
	if allSignaturesAbsent([]SignatureObservation{{Found: true}}) {
		t.Fatal("landed signature was treated as absent")
	}
	accounts := []ConfirmedAccount{{Address: "a", Owner: bridgeTokenProgram, Lamports: 1, Data: []byte{1, 2}}}
	if hashConfirmedAccounts(accounts) != hashConfirmedAccounts(accounts) {
		t.Fatal("protected account hash is not deterministic")
	}
}

func TestWriteR03EvidenceMatchesRuntimeVerifierShape(t *testing.T) {
	wire := append([]byte{1}, make([]byte, 64)...)
	plan := R03LifecyclePlan{
		Lane: SelectedRouteID, ProtectedAddresses: []string{"obligation"}, ObligationAddress: "obligation",
		Transactions: []R03LifecycleTransaction{
			{Role: "entry", Phase: "entry", PacketBytes: len(wire), TransactionBase64: base64.StdEncoding.EncodeToString(wire), SimulationPassed: true},
			{Role: "unwind", Phase: "unwind", PacketBytes: len(wire), TransactionBase64: base64.StdEncoding.EncodeToString(wire), SimulationPassed: true},
			{Role: "return", Phase: "return", PacketBytes: len(wire), TransactionBase64: base64.StdEncoding.EncodeToString(wire), SimulationPassed: true},
			{Role: "nav", Phase: "nav", PacketBytes: len(wire), TransactionBase64: base64.StdEncoding.EncodeToString(wire), SimulationPassed: true},
		},
		Hold: R03HoldRecord{Action: "HOLD", Reason: "capacity", ObservationID: "obs", Slot: 7},
	}
	output := t.TempDir() + "/evidence.json"
	result := R03BundleResult{ContextSlot: 7, SignatureAbsentOnChain: true, ChainPreStateSHA256: "same", ChainPostStateSHA256: "same"}
	if err := WriteR03Evidence(output, plan, result); err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	bytes, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["selectedLane"] != SelectedRouteID || evidence["signatureAbsentOnChain"] != true || evidence["chainPreStateSha256"] != evidence["chainPostStateSha256"] {
		t.Fatalf("unexpected verifier-facing R03 evidence: %#v", evidence)
	}
}
