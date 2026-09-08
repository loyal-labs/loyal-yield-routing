package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Real RPC serialization/decoding and reserve decoding, with controlled complete
// batches. Record every request so retries cannot silently lower the slot fence.
func observationTestWorker(t *testing.T, batch func(int) (int64, []Account)) (*Worker, []string, map[string]ReserveIdentity, *[]int64) {
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Params) != 2 {
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
			t.Error("retry changed the catalog or confirmation requirement")
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
	return &Worker{config: Config{SlotDuration: 400 * time.Millisecond}, rpc: NewRPCClient(server.URL)}, addresses, identities, &floors
}

func observationAccounts() []Account {
	return []Account{
		reserveFixture(ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}, 50_000_000_000_000, 50_000_000_000_000),
		reserveFixture(ReserveIdentity{Address: testIdentity(4), Market: testIdentity(40), Mint: USDCMint}, 50_000_000_000_000, 50_000_000_000_000),
	}
}

func TestReserveObservationReplacesWholeInconsistentBatch(t *testing.T) {
	for _, mode := range []Mode{ModeShadow, ModePublish} {
		t.Run(string(mode), func(t *testing.T) {
			w, addresses, identities, floors := observationTestWorker(t, func(attempt int) (int64, []Account) {
				accounts := observationAccounts()
				if attempt == 1 {
					binary.LittleEndian.PutUint64(accounts[1].Data[16:24], 1002)
					return 1000, accounts
				}
				// Change even the previously valid account: its old bytes must not survive.
				binary.LittleEndian.PutUint64(accounts[0].Data[224:232], 40_000_000_000_000)
				binary.LittleEndian.PutUint64(accounts[1].Data[16:24], 1002)
				return 1003, accounts
			})
			w.config.Mode = mode
			snapshot, err := w.observeConfirmedReserveCatalog(context.Background(), addresses, identities, 999)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*floors, []int64{999, 1002}) {
				t.Fatalf("slot floors: %v", *floors)
			}
			if len(snapshot.Reserves) != 2 || snapshot.Slot != 1003 {
				t.Fatalf("incomplete snapshot: %+v", snapshot)
			}
			for _, state := range snapshot.Reserves {
				if state.Slot != 1003 {
					t.Fatal("mixed observation slots")
				}
			}
			if snapshot.Reserves[addresses[0]].TotalSupplyUSDMicros != 90_000_000_000_000 {
				t.Fatal("reused account economics from rejected batch")
			}
		})
	}
}

func TestReserveObservationPersistentMismatchFailsClosed(t *testing.T) {
	w, addresses, identities, floors := observationTestWorker(t, func(attempt int) (int64, []Account) {
		a := observationAccounts()
		slot := int64(999 + attempt)
		binary.LittleEndian.PutUint64(a[0].Data[16:24], uint64(slot+1))
		return slot, a
	})
	snapshot, err := w.observeConfirmedReserveCatalog(context.Background(), addresses, identities, 1000)
	var mismatch *ReserveSlotOrderMismatch
	if !errors.As(err, &mismatch) || len(snapshot.Reserves) != 0 || snapshot.Slot != 0 {
		t.Fatalf("accepted inconsistent evidence: %+v %v", snapshot, err)
	}
	if !reflect.DeepEqual(*floors, []int64{1000, 1001, 1002}) {
		t.Fatalf("retry bound/floor violated: %v", *floors)
	}
	if mismatch.ContextSlot != 1002 || mismatch.LastUpdateSlot != 1003 {
		t.Fatalf("missing diagnostic slots: %+v", mismatch)
	}
}

func TestReserveObservationDoesNotRetryOtherInvalidEvidence(t *testing.T) {
	for _, failure := range []string{"identity", "layout", "obsolete", "below_minimum_slot"} {
		t.Run(failure, func(t *testing.T) {
			w, addresses, identities, floors := observationTestWorker(t, func(int) (int64, []Account) {
				a := observationAccounts()
				binary.LittleEndian.PutUint64(a[0].Data[16:24], 1002)
				switch failure {
				case "identity":
					a[1].Owner = "wrong-owner"
				case "layout":
					a[1].Data = a[1].Data[:100]
				case "obsolete":
					a[1].Data[reserveConfigOffset] = 1
				case "below_minimum_slot":
					return 998, a
				}
				return 1000, a
			})
			snapshot, err := w.observeConfirmedReserveCatalog(context.Background(), addresses, identities, 999)
			if err == nil || len(snapshot.Reserves) != 0 || len(*floors) != 1 {
				t.Fatalf("invalid evidence was accepted or retried: %v %v", *floors, err)
			}
		})
	}
}

func TestReserveObservationCancellationStopsBackoff(t *testing.T) {
	w, addresses, identities, floors := observationTestWorker(t, func(int) (int64, []Account) {
		a := observationAccounts()
		binary.LittleEndian.PutUint64(a[0].Data[16:24], 1001)
		return 1000, a
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	snapshot, err := w.observeConfirmedReserveCatalog(ctx, addresses, identities, 1000)
	if !errors.Is(err, context.DeadlineExceeded) || len(*floors) != 1 || len(snapshot.Reserves) != 0 {
		t.Fatalf("cancellation ignored: %v %v", *floors, err)
	}
}
