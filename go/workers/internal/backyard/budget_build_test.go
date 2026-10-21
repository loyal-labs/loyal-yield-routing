package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// Exercise the actual RPC decoder, production compiler and valuation path.
// The transport supplies controlled chain inputs; it rejects every signing,
// simulation and send RPC. This is a local negative witness, not live proof.
func budgetBuildRPC(t *testing.T, fee uint64, finalSlot int64) *chain.Client {
	return budgetBuildRPCWithAccounts(t, fee, finalSlot, nil)
}

func budgetBuildRPCWithAccounts(t *testing.T, fee uint64, finalSlot int64, extra []ConfirmedAccount) *chain.Client {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	sf := new(big.Int).Lsh(big.NewInt(1), 60)
	usdc := reserveFixture(t, config.DebtReserve, fleet.USDCMint, 42, sf, 1, 1)
	sol := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, 42, new(big.Int).Mul(sf, big.NewInt(100)), 1, 1)
	putKey(t, sol.Data[32:64], fleet.KaminoMainMarket)
	binary.LittleEndian.PutUint64(sol.Data[272:280], 9)
	for _, account := range []ConfirmedAccount{usdc, sol} {
		binary.LittleEndian.PutUint64(account.Data[264:272], 1000)
	}
	mint := func(address string, decimals byte) ConfirmedAccount {
		data := make([]byte, 82)
		data[44], data[45] = decimals, 1
		return ConfirmedAccount{Address: address, Owner: classicTokenProgram, Lamports: 1, Data: data}
	}
	clock := ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 40)}
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	accounts := map[string]ConfirmedAccount{}
	for _, a := range []ConfirmedAccount{usdc, sol, mint(fleet.USDCMint, 6), mint(budgetWrappedSOLMint, 9), clock} {
		accounts[a.Address] = a
	}
	for _, a := range extra {
		accounts[a.Address] = a
	}
	rpc := newFakeChain(t, nil)
	var reads atomic.Int64 // pricers read the slot from concurrent cost reads
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var result any
		switch body.Method {
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": bridgeVault, "lastValidBlockHeight": 99}}
		case "getSlot":
			result = int64(42)
			if reads.Add(1) > 1 {
				result = finalSlot
			}
		case "getFeeForMessage":
			var message string
			if err := json.Unmarshal(body.Params[0], &message); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(message)
			if err != nil || len(decoded) < 4 || (decoded[0] != 1 && (decoded[0] != 0x80 || decoded[1] != 1)) {
				t.Fatal("fee request must contain unsigned one-signer message")
			}
			result = map[string]any{"context": map[string]int64{"slot": finalSlot}, "value": fee}
		case "getProgramAccounts":
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": []any{}} // no withdrawal receipts
		case "getEpochInfo":
			result = finalizedEpoch(10) // A signed HOLD at this height is not expired in the DB fixture.
		case "simulateTransaction":
			result = simulatedCapture(t, body.Params, finalSlot, func(address string) (ConfirmedAccount, bool) { a, ok := accounts[address]; return a, ok })
		case "getMinimumBalanceForRentExemption":
			// Read-only fixture of the real RPC: (128+bytes) lamports per byte
			// year across the two-year exemption threshold at 3480 lamports.
			var size int
			if err := json.Unmarshal(body.Params[0], &size); err != nil || size < 0 {
				t.Fatal("rent exemption request must carry a byte size")
			}
			result = uint64((128 + size) * 3480 * 2)
		case "getMultipleAccounts":
			var addresses []string
			if err := json.Unmarshal(body.Params[0], &addresses); err != nil {
				t.Fatal(err)
			}
			values := make([]any, 0, len(addresses))
			for _, address := range addresses {
				if captured, ok := capturedPolicyAccount(address); ok {
					values = append(values, captured)
					continue
				}
				a, ok := accounts[address]
				if !ok || a.Lamports == 0 {
					values = append(values, nil)
					continue
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			result = map[string]any{"context": map[string]int64{"slot": finalSlot}, "value": values}
		default:
			t.Fatalf("unexpected RPC before cap rejection: %s", body.Method)
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		return response(string(encoded)), nil
	})
	return rpc
}

// budgetView is the view of budgetBuildRPC's valuation accounts.
func budgetView(t *testing.T) *View {
	t.Helper()
	return fixtureView(t, budgetBuildRPC(t, 5000, 42))
}

// A failed read stops the pricing pass as the serial sequence did: the reads
// after it are cancelled, and the reported failure is the first in step
// order, so a consequence never displaces its root cause.
func TestConcurrentReadsStopsAtTheFirstFailureInOrder(t *testing.T) {
	t.Parallel()
	stale := fmt.Errorf("stale confirmed slot")
	started := time.Now()
	err := concurrentReads(t.Context(),
		func(context.Context) error { return stale },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	)
	if err != stale || time.Since(started) > time.Second {
		t.Fatal("later read not cancelled", err, time.Since(started))
	}
	err = concurrentReads(t.Context(),
		func(context.Context) error { time.Sleep(50 * time.Millisecond); return stale },
		func(context.Context) error { return fmt.Errorf("fee_message_or_slot_mismatch") },
	)
	if err != stale {
		t.Fatal("later failure displaced the earlier one", err)
	}
	if err = concurrentReads(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatal("clean pass", err)
	}
}
