package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"math"
	"testing"

	sdk "github.com/gagliardetto/solana-go"
)

// Unit receipt cases test rejection logic; actual executed packet coverage is
// separately supplied by lookup_svm_test, never by these constructed metadata.
func lookupUnitRecovery(t *testing.T) (LookupAttempt, SignatureStatus, *LookupReceipt, LookupSnapshot) {
	t.Helper()
	f := readLookupFixture(t)
	intent := lookupFixtureIntent(f)
	wire, err := signLookupMutation(intent, sdk.Hash([32]byte{42}).String(), 1150, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &LookupReceipt{Signature: wire.TransactionSignature, Slot: 1000, Wire: wire.SignedTransaction, FeeLamports: 5000}
	for _, a := range tx.Message.AccountKeys {
		receipt.Addresses = append(receipt.Addresses, a.String())
		pre, post := uint64(0), uint64(0)
		if a.String() == f.Manager {
			pre, post = 10000000, 9993000
		}
		if a.String() == f.Table {
			post = 2000
		}
		receipt.PreLamports = append(receipt.PreLamports, pre)
		receipt.PostLamports = append(receipt.PostLamports, post)
	}
	snapshot := LookupSnapshot{Address: f.Table, Owner: lookupProgram, Slot: 1001, Authority: f.Manager, Addresses: intent.Extension, DeactivationSlot: math.MaxUint64, LastExtendedSlot: 1000}
	return LookupAttempt{Intent: intent, Wire: wire}, SignatureStatus{Found: true, Confirmed: true, Finalized: true, Slot: 1000}, receipt, snapshot
}
func TestLookupRecoveryRequiresOwnedFinalizedWarmedEffects(t *testing.T) {
	a, status, receipt, snapshot := lookupUnitRecovery(t)
	result, err := recoverLookup(a, status, receipt, snapshot)
	if err != nil || result.proof == nil || result.proof.state != LookupReconciled {
		t.Fatalf("exact proof: %+v %v", result, err)
	}
	for name, change := range map[string]func(*SignatureStatus, **LookupReceipt, *LookupSnapshot){
		"signature absent": func(s *SignatureStatus, r **LookupReceipt, b *LookupSnapshot) { s.Found = false },
		"confirmed only":   func(s *SignatureStatus, r **LookupReceipt, b *LookupSnapshot) { s.Finalized = false },
		"receipt missing":  func(s *SignatureStatus, r **LookupReceipt, b *LookupSnapshot) { *r = nil },
		"not warmed":       func(s *SignatureStatus, r **LookupReceipt, b *LookupSnapshot) { b.Slot = 1000 },
	} {
		t.Run(name, func(t *testing.T) {
			s, r, b := status, receipt, snapshot
			change(&s, &r, &b)
			got, err := recoverLookup(a, s, r, b)
			if err != nil || got.proof != nil || got.wait == "" {
				t.Fatalf("must remain held: %+v %v", got, err)
			}
		})
	}
	for name, change := range map[string]func(*LookupReceipt, *LookupSnapshot){
		"owner":        func(r *LookupReceipt, b *LookupSnapshot) { b.Owner = sdk.SystemProgramID.String() },
		"prefix order": func(r *LookupReceipt, b *LookupSnapshot) { b.Addresses = []string{b.Addresses[1], b.Addresses[0]} },
		"rent": func(r *LookupReceipt, b *LookupSnapshot) {
			r.PostLamports = append([]uint64(nil), r.PostLamports...)
			r.PostLamports[0]++
		},
		"packet": func(r *LookupReceipt, b *LookupSnapshot) { r.Wire = append([]byte(nil), r.Wire...); r.Wire[1] ^= 1 },
		"error":  func(r *LookupReceipt, b *LookupSnapshot) { r.Err = `{"InstructionError":[0,"InvalidArgument"]}` },
	} {
		t.Run(name, func(t *testing.T) {
			r, b := *receipt, snapshot
			change(&r, &b)
			if _, err := recoverLookup(a, status, &r, b); err == nil {
				t.Fatal("drift accepted")
			}
		})
	}
}
func TestLookupCloseUsesProducedSlotHashesNotElapsedEstimate(t *testing.T) {
	b := LookupSnapshot{Slot: 9000, DeactivationSlot: 1000, SlotHashes: []uint64{8999, 1000, 999}}
	if lookupCloseReady(b, 1000) {
		t.Fatal("numeric gap bypassed actual cooldown")
	}
	b.SlotHashes = []uint64{8999, 8998, 8997}
	if !lookupCloseReady(b, 1000) {
		t.Fatal("expired produced-slot membership held")
	}
	b.Slot = 1000
	if lookupCloseReady(b, 1000) {
		t.Fatal("same-slot close accepted")
	}
}
