package multiply

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func TestRootWalletClaimMatchesIndependentSVMReceipt(t *testing.T) {
	raw, err := os.ReadFile("testdata/svm-root-wallet-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Settings, RootAuthority, Vault, Source, Destination, Signature, RequestID                            string
		WireBase64                                                                                           string
		VaultIndex                                                                                           uint8
		AmountRaw, ConfirmedSlot, SourceBeforeRaw, SourceAfterRaw, DestinationBeforeRaw, DestinationAfterRaw uint64
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
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
	if err := ValidateWalletClaimReceipt(route, topology, fixture.RequestID, &receipt); err != nil {
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
			if err := ValidateWalletClaimReceipt(&r, topology, fixture.RequestID, &receiptCopy); err == nil {
				t.Fatal("receipt escaped saved request ownership")
			}
		})
	}
	executor, _, _ := testExecutor(t)
	if _, err := executor.EnsureExactPolicy(context.Background(), topology, &ActionPlan{Action: ActionClaim}, &BuiltOperation{}); err == nil {
		t.Fatal("delegate claim family admitted")
	}
}

// TestRootWalletClaimAcceptanceIsRustExact rebuilds the real SVM claim's
// Squads instruction under a fresh authority and varies one thing at a time,
// following balance-sweep-ata-monitor's retained_earn_max_claim test: Rust
// accepts any signer count, and rejects an extra outer account, a custody
// table that is not exactly the five custody keys, a policy payload, another
// settings account, another vault index and a second inner transfer.
func TestRootWalletClaimAcceptanceIsRustExact(t *testing.T) {
	raw, err := os.ReadFile("testdata/svm-root-wallet-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Settings, Vault, Source, Destination, RequestID, WireBase64                                          string
		VaultIndex                                                                                           uint8
		AmountRaw, ConfirmedSlot, SourceBeforeRaw, SourceAfterRaw, DestinationBeforeRaw, DestinationAfterRaw uint64
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	topology, err := DeriveEarnMaxTopology(mustKey(f.Settings), 320)
	if err != nil {
		t.Fatal(err)
	}
	route, err := NewRouteState("svm-claim", f.Settings, f.VaultIndex, f.Vault, 320, TokenBalance{Account: f.Source, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: f.SourceAfterRaw}, f.ConfirmedSlot-1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	route.Goal = GoalWithdraw
	route.Withdrawal = &Withdrawal{RequestID: f.RequestID, DestinationAccount: f.Destination, AmountRaw: f.AmountRaw, Status: WithdrawalClaimable}
	wire, _ := base64.StdEncoding.DecodeString(f.WireBase64)
	original, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	var squads solana.CompiledInstruction
	for _, ix := range original.Message.Instructions {
		if original.Message.AccountKeys[ix.ProgramIDIndex] == mustKey(SquadsProgram) {
			squads = ix
		}
	}
	canonical, err := squads.ResolveInstructionAccounts(&original.Message)
	if err != nil {
		t.Fatal(err)
	}
	balance := func(account string, amount uint64) TokenBalance {
		return TokenBalance{Account: account, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: amount}
	}
	// build signs the claim instruction (accounts, data) with a fresh
	// authority plus extra signers carried by a compute-budget instruction.
	build := func(t *testing.T, accounts func([]*solana.AccountMeta) []*solana.AccountMeta, data func([]byte) []byte, extraSigners int) *WalletClaimReceipt {
		t.Helper()
		authority := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{91}, 32))
		keys := []solana.PrivateKey{solana.PrivateKey(authority)}
		metas := make([]*solana.AccountMeta, len(canonical))
		for i, m := range canonical {
			c := *m
			metas[i] = &c
		}
		metas[2] = &solana.AccountMeta{PublicKey: solana.PublicKeyFromBytes(authority[32:]), IsSigner: true}
		instructions := []solana.Instruction{}
		if extraSigners > 0 {
			budget := solana.AccountMetaSlice{}
			for i := 0; i < extraSigners; i++ {
				key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(92 + i)}, 32))
				keys = append(keys, solana.PrivateKey(key))
				budget = append(budget, &solana.AccountMeta{PublicKey: solana.PublicKeyFromBytes(key[32:]), IsSigner: true})
			}
			instructions = append(instructions, solana.NewInstruction(solana.ComputeBudget, budget, []byte{2, 0, 0, 0, 0}))
		}
		instructions = append(instructions, solana.NewInstruction(mustKey(SquadsProgram), accounts(metas), data(append([]byte(nil), squads.Data...))))
		tx, err := solana.NewTransaction(instructions, original.Message.RecentBlockhash, solana.TransactionPayer(solana.PublicKeyFromBytes(authority[32:])))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			for i := range keys {
				if keys[i].PublicKey() == key {
					return &keys[i]
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		signed, err := tx.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return &WalletClaimReceipt{Signature: tx.Signatures[0].String(), ConfirmationState: "confirmed", ConfirmedSlot: f.ConfirmedSlot, Wire: signed,
			SourceBefore: balance(f.Source, f.SourceBeforeRaw), SourceAfter: balance(f.Source, f.SourceAfterRaw), DestinationBefore: balance(f.Destination, f.DestinationBeforeRaw), DestinationAfter: balance(f.Destination, f.DestinationAfterRaw)}
	}
	same := func(m []*solana.AccountMeta) []*solana.AccountMeta { return m }
	keep := func(d []byte) []byte { return d }
	if err := ValidateWalletClaimReceipt(route, topology, f.RequestID, build(t, same, keep, 2)); err != nil {
		t.Fatalf("three-signer claim refused; Rust accepts any signer count: %v", err)
	}
	for name, receipt := range map[string]*WalletClaimReceipt{
		"extra outer account": build(t, func(m []*solana.AccountMeta) []*solana.AccountMeta {
			return append(m, &solana.AccountMeta{PublicKey: fixtureKey(70)})
		}, keep, 0),
		"unrelated custody key": build(t, func(m []*solana.AccountMeta) []*solana.AccountMeta {
			for _, a := range m[3:] {
				if a.PublicKey == mustKey(USDCMint) {
					a.PublicKey = fixtureKey(71)
				}
			}
			return m
		}, keep, 0),
		"another settings": build(t, func(m []*solana.AccountMeta) []*solana.AccountMeta { m[0].PublicKey = fixtureKey(72); return m }, keep, 0),
		"policy payload":   build(t, same, func(d []byte) []byte { d[10] = 1; return d }, 0),
		"vault index one":  build(t, same, func(d []byte) []byte { d[8] = 1; return d }, 0),
		"second transfer": build(t, same, func(d []byte) []byte {
			payload := d[15:]
			twice := append([]byte{2}, append(append([]byte(nil), payload[1:]...), payload[1:]...)...)
			out := append(append([]byte(nil), d[:11]...), binary.LittleEndian.AppendUint32(nil, uint32(len(twice)))...)
			return append(out, twice...)
		}, 0),
	} {
		if err := ValidateWalletClaimReceipt(route, topology, f.RequestID, receipt); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
