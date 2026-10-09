package fleetexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"testing"
	"time"
)

type historyPages struct {
	pages [][]finalizedAddressSignature
	calls int
}

func (h *historyPages) FinalizedAddressSignatures(context.Context, string, string, int64) ([]finalizedAddressSignature, error) {
	if h.calls >= len(h.pages) {
		return nil, nil
	}
	p := h.pages[h.calls]
	h.calls++
	return p, nil
}
func TestCrossMintCustodyHistoryRequiresRecognizedAnchorAndRejectsRestoredBalance(t *testing.T) {
	anchor := int64(100)
	for _, tc := range []struct {
		name       string
		statuses   []finalizedAddressSignature
		recognized map[string]bool
		allow      bool
	}{
		{"recognized anchor", []finalizedAddressSignature{{Signature: "known", Slot: 100, ConfirmationStatus: "finalized"}}, map[string]bool{"known": true}, true},
		{"empty history", nil, map[string]bool{}, false},
		{"anchor pruned", []finalizedAddressSignature{{Signature: "old", Slot: 99, ConfirmationStatus: "finalized"}}, map[string]bool{}, false},
		{"external restored balance", []finalizedAddressSignature{{Signature: "external", Slot: 101, ConfirmationStatus: "finalized"}, {Signature: "known", Slot: 100, ConfirmationStatus: "finalized"}}, map[string]bool{"known": true}, false},
		{"unfinalized", []finalizedAddressSignature{{Signature: "known", Slot: 100, ConfirmationStatus: "confirmed"}}, map[string]bool{"known": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyCustodyHistory(context.Background(), &historyPages{pages: [][]finalizedAddressSignature{tc.statuses}}, "account", anchor, 101, tc.recognized, true)
			if (err == nil) != tc.allow {
				t.Fatalf("allow=%v err=%v", tc.allow, err)
			}
		})
	}
	// An exactly full first page must continue to its old boundary; duplicate
	// signature cursors cannot be mistaken for a complete history page.
	page := make([]finalizedAddressSignature, 1000)
	recognized := map[string]bool{}
	for i := range page {
		signature := fmt.Sprint(i)
		page[i] = finalizedAddressSignature{Signature: signature, Slot: 1100 - int64(i), ConfirmationStatus: "finalized"}
		recognized[signature] = true
	}
	h := &historyPages{pages: [][]finalizedAddressSignature{page, {{Signature: "anchor", Slot: 100, ConfirmationStatus: "finalized"}, {Signature: "older", Slot: 99, ConfirmationStatus: "finalized"}}}}
	recognized["anchor"] = true
	if _, err := verifyCustodyHistory(context.Background(), h, "account", 100, 1200, recognized, true); err != nil || h.calls != 2 {
		t.Fatalf("history pagination: %d %v", h.calls, err)
	}
}

func TestCrossMintTokenAccountUsesCanonicalProgramAndStrictBaseState(t *testing.T) {
	f := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(f.FeePayer)
	for _, mint := range []string{fleet.USDCMint, "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"} {
		program, err := canonicalCustodyTokenProgram(mint)
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 165)
		m := sdk.MustPublicKeyFromBase58(mint)
		copy(data[:32], m[:])
		copy(data[32:64], owner[:])
		data[108] = 1
		binary.LittleEndian.PutUint64(data[64:72], 42)
		a := fleet.Account{Owner: program, Lamports: 1, Data: data}
		if got, err := custodyTokenAmount(a, mint, owner.String()); err != nil || got != 42 {
			t.Fatalf("token account %d %v", got, err)
		}
		a.Data[108] = 2
		if _, err := custodyTokenAmount(a, mint, owner.String()); err == nil {
			t.Fatal("frozen custody accepted")
		}
		a.Data[108] = 1
		binary.LittleEndian.PutUint32(a.Data[72:76], 3)
		if _, err := custodyTokenAmount(a, mint, owner.String()); err == nil {
			t.Fatal("invalid delegate tag accepted")
		}
		binary.LittleEndian.PutUint32(a.Data[72:76], 0)
		if program != sdk.TokenProgramID.String() {
			a.Data = append(a.Data, 2, 0, 0, 0, 0)
			if _, err := custodyTokenAmount(a, mint, owner.String()); err != nil {
				t.Fatal(err)
			}
			a.Data[165] = 1
			if _, err := custodyTokenAmount(a, mint, owner.String()); err == nil {
				t.Fatal("Token-2022 mint envelope accepted as account")
			}
		}
	}
}

func TestPreparedALTEpochsUsesLegacyCamelKeysAndRequiresExplicitZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"tables":[{"TableID":1,"MutationEpoch":0}]}`, `{"tables":[{"tableId":1}]}`, `{"tables":[{"tableId":1,"mutationEpoch":null}]}`, `{"tables":[{"tableId":1,"mutationEpoch":0},{"tableId":1,"mutationEpoch":0}]}`} {
		if _, err := parsePreparedALTEpochs([]byte(raw)); err == nil {
			t.Fatalf("invalid ALT proof accepted: %s", raw)
		}
	}
	if _, err := parsePreparedALTEpochs([]byte(`{"tables":[{"tableId":1,"mutationEpoch":0}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedFamilyClaimLeavesRustLeaseUnchanged(t *testing.T) {
	for _, family := range []string{"cross_mint", "voltr"} {
		t.Run(family, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			b := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()))
			wire, _ := integrationWire(t, 126)
			input := fixturePersistInput(t, ctx, pool, b, wire)
			id, _, err := store.PersistSignedRoute(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if family == "cross_mint" {
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET movement_route='cross_mint_jupiter',status='confirming',source_reserve='source',target_reserve='target',active_target_reserve='target',amount_raw=1000,custody_amount_raw=1000,custody_account='source',cross_mint_activation_control_generation=1,custody_mint='EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',source_liquidity_mint='EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',target_liquidity_mint='Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB' WHERE id=$1`, b.DecisionID); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, _ := json.Marshal(map[string]any{"route_kind": "same_mint", "source_kind": "voltr_manager"})
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET execution_plan=$2 WHERE id=$1`, b.OpportunityID, raw); err != nil {
					t.Fatal(err)
				}
			}
			leases, err := store.ClaimRecoveryWork(ctx, b.Cluster, "go-worker", time.Minute, 8)
			if err != nil {
				t.Fatal(err)
			}
			if len(leases) != 0 {
				t.Fatal("Go claimed an unsupported Rust-owned family")
			}
			_, _, owner, _, _ := durableRow(t, ctx, pool, id)
			if owner != nil {
				t.Fatalf("Go stole unsupported lease: %s", *owner)
			}
		})
	}
}
