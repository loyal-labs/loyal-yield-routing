package earn

import (
	"context"
	"strings"
	"testing"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

// Rejection paths never reach the store, so a nil store proves the handler
// validates before any persistence attempt.
func newValidatingHandler(t *testing.T) *Handler {
	t.Helper()
	handler := NewHandler(nil, "mainnet")
	handler.SetWatchSet(&watch.Set{Vaults: []watch.Vault{{
		Environment: "mainnet", Settings: "settings", Vault: "vault", VaultIndex: 1,
		Accounts: []watch.Account{{Pubkey: "watched", Role: "policy"}},
	}}})
	return handler
}

func accountUpdate(slot uint64, pubkey []byte, lamports uint64, signature []byte) *pb.SubscribeUpdate {
	return &pb.SubscribeUpdate{
		Filters: []string{watch.EarnPolicyAccounts},
		UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{
			Slot:    slot,
			Account: &pb.SubscribeUpdateAccountInfo{Pubkey: pubkey, Lamports: lamports, TxnSignature: signature},
		}},
	}
}

func TestHandlerRejectsAccountUpdateWithoutRoutablePayload(t *testing.T) {
	handler := newValidatingHandler(t)
	update := &pb.SubscribeUpdate{
		Filters:     []string{watch.EarnPolicyAccounts},
		UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: 10}},
	}
	_, err := handler.HandleAccount(context.Background(), update)
	if err == nil || !strings.Contains(err.Error(), "omitted the account payload") {
		t.Fatalf("payload-less update = %v, want explicit rejection", err)
	}
}

func TestHandlerRejectsMalformedAccountIdentityAndSignature(t *testing.T) {
	handler := newValidatingHandler(t)
	shortKey := make([]byte, 31)
	if _, err := handler.HandleAccount(context.Background(), accountUpdate(10, shortKey, 1, nil)); err == nil || !strings.Contains(err.Error(), "31 bytes") {
		t.Fatalf("short pubkey = %v, want byte-length rejection", err)
	}
	// A present but non-64-byte signature would corrupt the durable event key's
	// dedupe identity, so it must be rejected before enqueueing.
	if _, err := handler.HandleAccount(context.Background(), accountUpdate(10, make([]byte, 32), 1, make([]byte, 63))); err == nil || !strings.Contains(err.Error(), "signature has 63 bytes") {
		t.Fatalf("short signature = %v, want rejection", err)
	}
}

func TestHandlerRejectsZeroAndOversizedSlots(t *testing.T) {
	handler := newValidatingHandler(t)
	if _, err := handler.HandleAccount(context.Background(), accountUpdate(0, make([]byte, 32), 1, nil)); err == nil || !strings.Contains(err.Error(), "slot is invalid") {
		t.Fatalf("zero slot = %v, want rejection", err)
	}
}
