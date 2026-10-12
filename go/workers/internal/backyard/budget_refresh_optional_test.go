package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// The selector destination batch pins lifecycle addresses that may legitimately
// be absent (an unopened obligation or farm user state). A simulated reserve
// refresh over that batch must keep absent optional accounts absent — the same
// zero-value shape confirmedAccounts returns — while every
// required address still resolves.
func TestSimulatedReserveRefreshOptionalCapturePreservesPinnedAbsence(t *testing.T) {
	t.Parallel()
	client := newFakeChain(t, nil)
	rpcOf(client).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch payload.Method {
		case "getLatestBlockhash":
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
		case "simulateTransaction":
			populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
			body := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[null,` + populated + `,null]}}}`
			return response(body), nil
		default:
			t.Fatalf("unexpected RPC method %q", payload.Method)
			return nil, nil
		}
	})
	addresses := []string{budgetClockAddress, bridgeVault, bridgeStrategy}
	slot, accounts, err := simulateBudgetReserveRefreshOptional(context.Background(), client, PhaseOneLaneID, addresses, []string{budgetClockAddress, bridgeStrategy}, 41)
	if err != nil || slot != 43 {
		t.Fatalf("slot=%d err=%v", slot, err)
	}
	for _, index := range []int{0, 2} {
		if accounts[index].Address != addresses[index] || accounts[index].Lamports != 0 || accounts[index].Owner != "" || accounts[index].Data != nil {
			t.Fatalf("optional absence was not preserved at %d: %+v", index, accounts[index])
		}
	}
	if accounts[1].Lamports != 9 || accounts[1].Data == nil {
		t.Fatalf("required capture corrupted: %+v", accounts[1])
	}
}

// Absence stays fail-closed everywhere it was never explicitly permitted: the
// optional-aware capture still rejects a null required account, and the strict
// price consumer keeps rejecting any null at all.
func TestSimulatedReserveRefreshCaptureStaysFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		optional  bool
		nullIndex int
	}{
		{name: "optional_capture_rejects_required_null", optional: true, nullIndex: 1},
		{name: "strict_consumer_rejects_any_null", optional: false, nullIndex: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := newFakeChain(t, nil)
			rpcOf(client).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var payload struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				switch payload.Method {
				case "getLatestBlockhash":
					return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
				case "simulateTransaction":
					populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
					values := []string{populated, populated}
					values[testCase.nullIndex] = "null"
					body := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[` + values[0] + `,` + values[1] + `]}}}`
					return response(body), nil
				default:
					t.Fatalf("unexpected RPC method %q", payload.Method)
					return nil, nil
				}
			})
			addresses := []string{budgetClockAddress, bridgeVault}
			var captureErr error
			if testCase.optional {
				_, _, captureErr = simulateBudgetReserveRefreshOptional(context.Background(), client, PhaseOneLaneID, addresses, []string{budgetClockAddress}, 41)
			} else {
				_, _, captureErr = simulateBudgetReserveRefresh(context.Background(), client, PhaseOneLaneID, addresses, 41)
			}
			var hold *BudgetHold
			if !errors.As(captureErr, &hold) || hold.Reason != "price_refresh_capture_incomplete" {
				t.Fatalf("unexpected error: %v", captureErr)
			}
		})
	}
}

// The live RPC only returns accounts named in the message, rejecting the rest
// with -32602. The compiled refresh message must therefore carry every
// requested capture address so one simulation returns the full batch.
func TestSimulatedReserveRefreshMessageCarriesCaptureAccounts(t *testing.T) {
	t.Parallel()
	client := newFakeChain(t, nil)
	rpcOf(client).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload.Method == "getLatestBlockhash" {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
		}
		if payload.Method != "simulateTransaction" {
			t.Fatalf("unexpected RPC method %q", payload.Method)
		}
		var wire string
		var config struct {
			Accounts struct {
				Addresses []string `json:"addresses"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(payload.Params[0], &wire); err != nil {
			t.Fatalf("decode wire param: %v", err)
		}
		if err := json.Unmarshal(payload.Params[1], &config); err != nil {
			t.Fatalf("decode config param: %v", err)
		}
		raw, err := base64.StdEncoding.DecodeString(wire)
		if err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		keys, err := messageKeys(wireMsg(raw)) // the message, then its all-zero placeholder signature
		if err != nil {
			t.Fatal(err)
		}
		numAccounts := len(keys)
		messageKeys := map[publicKey]bool{}
		for _, key := range keys {
			messageKeys[key] = true
		}
		for _, address := range config.Accounts.Addresses {
			key, err := decodeKey(address)
			if err != nil || !messageKeys[key] {
				t.Fatalf("requested account %s is not a message key: %v", address, err)
			}
		}
		if len(config.Accounts.Addresses) > numAccounts {
			return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Too many accounts provided"}}`), nil
		}
		populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
		body2 := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[` + populated + `,null]}}}`
		return response(body2), nil
	})
	slot, accounts, err := simulateBudgetReserveRefreshOptional(context.Background(), client, PhaseOneLaneID, []string{bridgeVault, budgetClockAddress}, []string{budgetClockAddress}, 41)
	if err != nil || slot != 43 {
		t.Fatalf("slot=%d err=%v", slot, err)
	}
	if accounts[0].Lamports != 9 || accounts[1].Data != nil || accounts[1].Address != budgetClockAddress {
		t.Fatalf("capture snapshot misaligned: %+v", accounts)
	}
}

// Two held AUTO lanes share their collateral reserve, so their valuation
// refresh is five RefreshReserves (AUTO, PYUSD, wYLDS and the two USDC
// reference reserves). 2026-10-11: the legacy encoder capped every message at
// four, so the first tranche into AUTO/AUTO/wYLDS held on kamino_stale after
// its release landed.
func TestTwoAUTOLaneValuationRefreshSimulatesEveryReserve(t *testing.T) {
	t.Parallel()
	routes := []RuntimeRoute{autoAUTOWYLDS, autoAUTOPYUSD}
	addresses, err := overflowRoomAddresses(autoAUTOWYLDS)
	if err != nil {
		t.Fatal(err)
	}
	reserves := map[publicKey]bool{}
	for _, route := range routes {
		for _, reserve := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
			reserves[mustKey(reserve)] = true
		}
	}
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	reserves[mustKey(reference.CollateralReserve)], reserves[mustKey(reference.DebtReserve)] = true, true
	client := newFakeChain(t, nil)
	rpcOf(client).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.Method != "simulateTransaction" {
			t.Fatalf("unexpected RPC request %q: %v", payload.Method, err)
		}
		var wire string
		if err := json.Unmarshal(payload.Params[0], &wire); err != nil {
			t.Fatalf("decode wire param: %v", err)
		}
		raw, err := base64.StdEncoding.DecodeString(wire)
		if err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		instructions := decodeTestInstructions(t, wireMsg(raw))
		count := len(instructions)
		refreshed := map[publicKey]bool{}
		for i, ix := range instructions {
			if ix.ProgramID != kamino.ProgramID || !bytesEqual(ix.Data, kamino.RefreshReserveDiscriminator[:]) {
				t.Fatalf("instruction %d is not a reserve refresh", i)
			}
			refreshed[publicKey(ix.Accounts[0].PublicKey)] = true
		}
		if count != len(reserves) || len(refreshed) != len(reserves) {
			t.Fatalf("refreshed %d reserves in %d instructions, want each of %d once", len(refreshed), count, len(reserves))
		}
		for reserve := range reserves {
			if !refreshed[reserve] {
				t.Fatalf("reserve %s was not refreshed", encodeBase58(reserve[:]))
			}
		}
		populated := `{"owner":"KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD","lamports":9,"data":["AQ==","base64"],"executable":false}`
		result := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[`
		for i := range addresses {
			if i > 0 {
				result += ","
			}
			result += populated
		}
		return response(result + `]}}}`), nil
	})
	slot, accounts, err := simulateRouteValuationRefresh(context.Background(), client, routes, addresses, 41)
	if err != nil || slot != 43 || len(accounts) != len(addresses) {
		t.Fatalf("two-lane refresh: slot=%d accounts=%d err=%v", slot, len(accounts), err)
	}
}
