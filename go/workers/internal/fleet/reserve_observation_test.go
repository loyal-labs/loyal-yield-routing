package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Real RPC serialization/decoding and reserve decoding, with controlled complete
// batches. Record every request's slot floor.
func observationTestWorker(t *testing.T, batch func(int) (int64, []chain.Account)) (*Worker, []string, map[string]ReserveIdentity, *[]int64) {
	t.Helper()
	a := ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}
	b := ReserveIdentity{Address: testIdentity(4), Market: testIdentity(40), Mint: USDCMint}
	addresses := []string{a.Address, b.Address}
	identities := map[string]ReserveIdentity{a.Address: a, b.Address: b}
	floors := []int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Method == "getRecentPerformanceSamples" {
			json.NewEncoder(w).Encode(map[string]any{"result": testPerformanceSamples})
			return
		} else if err != nil || len(req.Params) != 2 {
			t.Error("invalid RPC request")
			w.WriteHeader(400)
			return
		}
		var requested []string
		json.Unmarshal(req.Params[0], &requested)
		var opts struct {
			Commitment string `json:"commitment"`
			Min        int64  `json:"minContextSlot"`
		}
		json.Unmarshal(req.Params[1], &opts)
		if req.Method != "getMultipleAccounts" || opts.Commitment != "confirmed" || !reflect.DeepEqual(requested, addresses) {
			t.Error("observation changed the catalog or confirmation requirement")
		}
		floors = append(floors, opts.Min)
		slot, accounts := batch(len(floors))
		values := make([]any, len(accounts))
		for i, a := range accounts {
			values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"context": map[string]any{"slot": slot}, "value": values}})
	}))
	t.Cleanup(server.Close)
	return &Worker{rpc: testChain(t, server.URL)}, addresses, identities, &floors
}

func observationAccounts() []chain.Account {
	return []chain.Account{
		reserveFixture(ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}, 50_000_000_000_000, 50_000_000_000_000),
		reserveFixture(ReserveIdentity{Address: testIdentity(4), Market: testIdentity(40), Mint: USDCMint}, 50_000_000_000_000, 50_000_000_000_000),
	}
}

// getMultipleAccounts returns one bank's state: a reserve refreshed after the
// response's context slot is valid evidence at its own refresh slot, read once.
func TestReserveObservationAcceptsReserveRefreshedAfterContextSlot(t *testing.T) {
	w, addresses, identities, floors := observationTestWorker(t, func(int) (int64, []chain.Account) {
		accounts := observationAccounts()
		binary.LittleEndian.PutUint64(accounts[1].Data[16:24], 1002)
		return 1000, accounts
	})
	snapshot, err := w.observeConfirmedReserveCatalog(context.Background(), addresses, identities, 999)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*floors, []int64{999}) || snapshot.Slot != 1000 || len(snapshot.Reserves) != 2 {
		t.Fatalf("observation: floors=%v snapshot=%+v", *floors, snapshot)
	}
	if held := snapshot.Reserves[addresses[0]]; held.Slot != 1000 || held.LastUpdateSlot != 900 || held.EconomicSlotLag != 100 {
		t.Fatalf("context-slot reserve: %+v", held)
	}
	ahead := snapshot.Reserves[addresses[1]]
	if ahead.Slot != 1002 || ahead.LastUpdateSlot != 1002 || ahead.EconomicSlotLag != 0 || ahead.EconomicLifetimeMillis <= 0 {
		t.Fatalf("refreshed reserve must be read at its own refresh slot: %+v", ahead)
	}
}

func TestReserveObservationRejectsInvalidEvidence(t *testing.T) {
	for _, failure := range []string{"identity", "layout", "obsolete", "below_minimum_slot"} {
		t.Run(failure, func(t *testing.T) {
			w, addresses, identities, floors := observationTestWorker(t, func(int) (int64, []chain.Account) {
				a := observationAccounts()
				switch failure {
				case "identity":
					a[1].Owner = solana.SystemProgramID
				case "layout":
					a[1].Data = a[1].Data[:100]
				case "obsolete":
					a[1].Data[4856] = 1
				case "below_minimum_slot":
					return 998, a
				}
				return 1000, a
			})
			snapshot, err := w.observeConfirmedReserveCatalog(context.Background(), addresses, identities, 999)
			if err == nil || len(snapshot.Reserves) != 0 || len(*floors) != 1 {
				t.Fatalf("invalid evidence was accepted or re-read: %v %v", *floors, err)
			}
		})
	}
}
