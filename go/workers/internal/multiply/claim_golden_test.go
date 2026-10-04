package multiply

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRootWalletClaimMatchesIndependentSVMReceipt(t *testing.T) {
	raw, err := os.ReadFile("testdata/svm-root-wallet-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Provenance struct {
			ProgramSHA256 string `json:"programSha256"`
		} `json:"provenance"`
		Settings, RootAuthority, Vault, Source, Destination, Signature, RequestID                            string
		WireBase64                                                                                           string
		VaultIndex                                                                                           uint8
		AmountRaw, ConfirmedSlot, SourceBeforeRaw, SourceAfterRaw, DestinationBeforeRaw, DestinationAfterRaw uint64
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile("../../../../crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so")
	if err != nil {
		t.Fatal(err)
	}
	programHash := sha256.Sum256(program)
	if fixture.Provenance.ProgramSHA256 != hex.EncodeToString(programHash[:]) {
		t.Fatal("SVM fixture program provenance drifted")
	}
	topology, err := DeriveEarnMaxTopology(mustKey(fixture.Settings), 320)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Vault.String() != fixture.Vault || topology.VaultIndex != fixture.VaultIndex || topology.ClaimCustody.String() != fixture.Source {
		t.Fatal("Go topology differs from actual Squads SVM vault and custody")
	}
	route, err := NewRouteState("svm-claim", fixture.Settings, fixture.VaultIndex, fixture.Vault, 320, TokenBalance{Account: fixture.Source, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: fixture.SourceAfterRaw}, fixture.ConfirmedSlot-1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	route.Goal = GoalWithdraw
	route.Withdrawal = &Withdrawal{RequestID: fixture.RequestID, DestinationAccount: fixture.Destination, AmountRaw: fixture.AmountRaw, Status: WithdrawalClaimable}
	balance := func(account string, amount uint64) TokenBalance {
		return TokenBalance{Account: account, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: amount}
	}
	wire, err := base64.StdEncoding.DecodeString(fixture.WireBase64)
	if err != nil {
		t.Fatal(err)
	}
	receipt := WalletClaimReceipt{Signature: fixture.Signature, ConfirmationState: "confirmed", ConfirmedSlot: fixture.ConfirmedSlot, Wire: wire, SourceBefore: balance(fixture.Source, fixture.SourceBeforeRaw), SourceAfter: balance(fixture.Source, fixture.SourceAfterRaw), DestinationBefore: balance(fixture.Destination, fixture.DestinationBeforeRaw), DestinationAfter: balance(fixture.Destination, fixture.DestinationAfterRaw)}
	root := mustKey(fixture.RootAuthority)
	if err := ValidateWalletClaimReceipt(route, topology, fixture.RequestID, root, &receipt); err != nil {
		t.Fatalf("real Squads/SPL receipt refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RouteState, *WalletClaimReceipt)
	}{
		{"wrong-request", func(r *RouteState, _ *WalletClaimReceipt) { r.Withdrawal.RequestID = "other-request" }},
		{"saved-destination-drift", func(r *RouteState, _ *WalletClaimReceipt) { r.Withdrawal.DestinationAccount = fixtureKey(85).String() }},
		{"saved-amount-drift", func(r *RouteState, _ *WalletClaimReceipt) { r.Withdrawal.AmountRaw++ }},
		{"short-payout", func(_ *RouteState, r *WalletClaimReceipt) { r.DestinationAfter.AmountRaw-- }},
		{"wrong-mint", func(_ *RouteState, r *WalletClaimReceipt) { r.DestinationAfter.Mint = fixtureKey(88).String() }},
		{"unknown-confirmation", func(_ *RouteState, r *WalletClaimReceipt) { r.ConfirmationState = "processed" }},
		{"stale-receipt", func(r *RouteState, _ *WalletClaimReceipt) { r.ObservedSlot = fixture.ConfirmedSlot + 1 }},
		{"signature-mismatch", func(_ *RouteState, r *WalletClaimReceipt) { r.Signature = fixtureKey(86).String() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := *route
			w := *route.Withdrawal
			r.Withdrawal = &w
			receiptCopy := receipt
			tc.mutate(&r, &receiptCopy)
			if err := ValidateWalletClaimReceipt(&r, topology, fixture.RequestID, root, &receiptCopy); err == nil {
				t.Fatal("receipt escaped saved request ownership")
			}
		})
	}
	if err := ValidateWalletClaimReceipt(route, topology, fixture.RequestID, fixtureKey(87), &receipt); err == nil {
		t.Fatal("non-root signer accepted")
	}
	executor, _, _ := testExecutor(t)
	if _, err := executor.EnsureExactPolicy(context.Background(), topology, &ActionPlan{Action: ActionClaim}, &BuiltOperation{}); err == nil {
		t.Fatal("delegate claim family admitted")
	}
}
