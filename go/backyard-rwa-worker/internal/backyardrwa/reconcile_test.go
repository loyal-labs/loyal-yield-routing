package backyardrwa

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestConfirmedTransactionReconciliationUsesReceiptDeltasAndAdaptorReturn(t *testing.T) {
	mint, authority := testPublicKey(11), testPublicKey(44)
	a, b := testPublicKey(77), testPublicKey(99)
	returnBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(returnBytes, 123)
	expected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: a, Owner: classicTokenProgram, Mint: mint, Authority: authority, BeforeRaw: 10, AfterRaw: 7},
			{Address: b, Owner: classicTokenProgram, Mint: mint, Authority: authority, BeforeRaw: 1, AfterRaw: 4},
		},
		ReturnData: &ExpectedReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: base64.StdEncoding.EncodeToString(returnBytes)},
	}
	receipt := ConfirmedTransactionEvidence{
		Signature: "persisted-signature", Slot: 77,
		PreTokenBalances: []TransactionTokenBalance{
			{Address: a, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 10},
			{Address: b, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 1},
		},
		PostTokenBalances: []TransactionTokenBalance{
			{Address: b, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 4},
			{Address: a, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 7},
		},
		ReturnData: &ProgramReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: expected.ReturnData.DataBase64},
	}
	reconciliation, evidence, err := ReconcileConfirmedTransaction(expected, receipt)
	if err != nil || reconciliation.ConfirmedSlot != 77 || !reconciliation.Conserved ||
		!strings.Contains(string(evidence), `"source":"confirmed-transaction-meta"`) ||
		!strings.Contains(string(evidence), `"signature":"persisted-signature"`) {
		t.Fatalf("reconciliation=%+v evidence=%s err=%v", reconciliation, evidence, err)
	}
	// A concurrent later account mutation is deliberately not an input to this
	// API; the immutable receipt remains sufficient and deterministic.
	receipt.PostTokenBalances[0].Raw = 5
	if _, _, err := ReconcileConfirmedTransaction(expected, receipt); err == nil {
		t.Fatal("receipt postcondition drift was accepted")
	}
}

func TestConfirmedTransactionReconciliationRejectsReturnDataDrift(t *testing.T) {
	mint, authority, address := testPublicKey(11), testPublicKey(44), testPublicKey(77)
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 8))
	expected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts:   []ExpectedAccountEffect{{Address: address, Owner: classicTokenProgram, Mint: mint, Authority: authority, BeforeRaw: 1, AfterRaw: 1}},
		ReturnData: &ExpectedReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: encoded},
	}
	receipt := ConfirmedTransactionEvidence{
		Signature: "signature", Slot: 1,
		PreTokenBalances:  []TransactionTokenBalance{{Address: address, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 1}},
		PostTokenBalances: []TransactionTokenBalance{{Address: address, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 1}},
		ReturnData:        &ProgramReturnData{ProgramID: testPublicKey(3), DataBase64: encoded},
	}
	if _, _, err := ReconcileConfirmedTransaction(expected, receipt); err == nil {
		t.Fatal("wrong adaptor return-data program was accepted")
	}
}

func TestConfirmedTransactionReconciliationAcceptsExactRuntimeReturnLog(t *testing.T) {
	mint, authority, address := testPublicKey(11), testPublicKey(44), testPublicKey(77)
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 8))
	expected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts:   []ExpectedAccountEffect{{Address: address, Owner: classicTokenProgram, Mint: mint, Authority: authority, BeforeRaw: 1, AfterRaw: 1}},
		ReturnData: &ExpectedReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: encoded},
	}
	receipt := ConfirmedTransactionEvidence{
		Signature: "signature", Slot: 1,
		PreTokenBalances:  []TransactionTokenBalance{{Address: address, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 1}},
		PostTokenBalances: []TransactionTokenBalance{{Address: address, OwnerProgram: classicTokenProgram, Mint: mint, Authority: authority, Raw: 1}},
		Logs:              []string{"Program return: " + bridgeAdaptorProgram + " " + encoded},
	}
	if _, _, err := ReconcileConfirmedTransaction(expected, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Logs[0] = "Program log: Program return: " + bridgeAdaptorProgram + " " + encoded
	if _, _, err := ReconcileConfirmedTransaction(expected, receipt); err == nil {
		t.Fatal("spoofable program log was accepted as runtime return data")
	}
}

func TestDecodeExpectedEffectsFromOperationEnvelope(t *testing.T) {
	mint, authority, address := testPublicKey(11), testPublicKey(44), testPublicKey(77)
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{{Address: address, Owner: classicTokenProgram, Mint: mint, Authority: authority}}}
	encoded, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "expectedEffects": expected})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeExpectedEffects(encoded)
	if err != nil || len(decoded.Accounts) != 1 || decoded.Accounts[0].Address != address {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if _, err := DecodeExpectedEffects([]byte(`{"schema":"loyal-backyard-rwa-operation-evidence/v1","expectedEffects":null}`)); err == nil {
		t.Fatal("unbuilt operation accepted as reconcilable effects")
	}
}
