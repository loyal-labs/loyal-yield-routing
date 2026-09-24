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
	receipt.ReturnData = &ProgramReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: base64.StdEncoding.EncodeToString([]byte("different"))}
	if _, _, err := ReconcileConfirmedTransaction(expected, receipt); err == nil {
		t.Fatal("matching runtime log overrode contradictory metadata return data")
	}
	receipt.ReturnData = nil
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

// A user deposit (or claim) that lands between the worker's observation and a
// REPORT_NAV must not fail reconciliation: Voltr idle is reconciled by this
// transaction's own delta. Every other account, and a nonzero delta on idle,
// stays exact. Regression for the 2026-09-24 out_of_band_crank halt (ASK-2304).
func TestReconciliationToleratesConcurrentUserChangeOnVoltrIdleOnly(t *testing.T) {
	strategyAuth, strategyATA := bridgeStrategyAuth, bridgeStrategyATA
	effects := func(idleBefore, idleAfter uint64) ExpectedEffects {
		return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
			{Address: bridgeIdleATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeIdleAuthority, BeforeRaw: idleBefore, AfterRaw: idleAfter},
			{Address: strategyATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: strategyAuth, BeforeRaw: 0, AfterRaw: 0},
		}}
	}
	receipt := func(idlePre, idlePost, strategyPre, strategyPost uint64) ConfirmedTransactionEvidence {
		return ConfirmedTransactionEvidence{Signature: "report", Slot: 450085751,
			PreTokenBalances: []TransactionTokenBalance{
				{Address: bridgeIdleATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeIdleAuthority, Raw: idlePre},
				{Address: strategyATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: strategyAuth, Raw: strategyPre},
			},
			PostTokenBalances: []TransactionTokenBalance{
				{Address: bridgeIdleATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeIdleAuthority, Raw: idlePost},
				{Address: strategyATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: strategyAuth, Raw: strategyPost},
			}}
	}
	// The incident: observed 95,387,976; a 5 USDC deposit landed first.
	if _, _, err := ReconcileConfirmedTransaction(effects(95_387_976, 95_387_976), receipt(100_387_976, 100_387_976, 0, 0)); err != nil {
		t.Fatalf("zero-delta report after a concurrent user deposit was rejected: %v", err)
	}
	// The report itself must still not move idle.
	if _, _, err := ReconcileConfirmedTransaction(effects(95_387_976, 95_387_976), receipt(100_387_976, 100_387_975, 0, 0)); err == nil {
		t.Fatal("idle moved by the transaction itself was accepted")
	}
	// A capital move keeps its exact delta even when the base drifted.
	if _, _, err := ReconcileConfirmedTransaction(effects(10, 7), receipt(15, 12, 0, 0)); err == nil {
		t.Fatal("non-conserved allocation delta was accepted") // strategy did not receive the 3
	}
	// Non-idle accounts keep the exact precondition.
	if _, _, err := ReconcileConfirmedTransaction(effects(95_387_976, 95_387_976), receipt(95_387_976, 95_387_976, 1, 1)); err == nil {
		t.Fatal("strategy custody precondition drift was accepted")
	}
}
