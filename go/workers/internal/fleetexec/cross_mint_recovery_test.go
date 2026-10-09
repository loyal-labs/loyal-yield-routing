package fleetexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type historyPages struct {
	pages [][]chain.Signed
	calls int
}

func (h *historyPages) History(context.Context, sdk.PublicKey, int, sdk.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error) {
	if h.calls >= len(h.pages) {
		return nil, nil
	}
	p := h.pages[h.calls]
	h.calls++
	return p, nil
}

// signed is one finalized history entry for the named test signature.
func signed(name string, slot uint64) chain.Signed {
	return chain.Signed{Signature: testSignature(name), Slot: slot}
}

func recognizedSignatures(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[testSignature(name).String()] = true
	}
	return out
}

func TestCrossMintCustodyHistoryRequiresRecognizedAnchorAndRejectsRestoredBalance(t *testing.T) {
	anchor := int64(100)
	account := sdk.SystemProgramID.String()
	for _, tc := range []struct {
		name       string
		statuses   []chain.Signed
		recognized map[string]bool
		allow      bool
	}{
		{"recognized anchor", []chain.Signed{signed("known", 100)}, recognizedSignatures("known"), true},
		{"empty history", nil, map[string]bool{}, false},
		{"anchor pruned", []chain.Signed{signed("old", 99)}, map[string]bool{}, false},
		{"external restored balance", []chain.Signed{signed("external", 101), signed("known", 100)}, recognizedSignatures("known"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyCustodyHistory(context.Background(), &historyPages{pages: [][]chain.Signed{tc.statuses}}, account, anchor, 101, tc.recognized, true)
			if (err == nil) != tc.allow {
				t.Fatalf("allow=%v err=%v", tc.allow, err)
			}
		})
	}
	// An exactly full first page must continue to its old boundary; duplicate
	// signature cursors cannot be mistaken for a complete history page.
	page := make([]chain.Signed, 1000)
	recognized := recognizedSignatures("anchor")
	for i := range page {
		page[i] = signed(fmt.Sprint(i), uint64(1100-i))
		recognized[page[i].Signature.String()] = true
	}
	h := &historyPages{pages: [][]chain.Signed{page, {signed("anchor", 100), signed("older", 99)}}}
	if _, err := verifyCustodyHistory(context.Background(), h, account, 100, 1200, recognized, true); err != nil || h.calls != 2 {
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
		a := &chain.Account{Owner: sdk.MustPublicKeyFromBase58(program), Lamports: 1, Data: data}
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
