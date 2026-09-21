package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual RPC decoder, production compiler and valuation path.
// The transport supplies controlled chain inputs; it rejects every signing,
// simulation and send RPC. This is a local negative witness, not live proof.
func budgetBuildRPC(t *testing.T, fee uint64, finalSlot int64) *RPCClient {
	return budgetBuildRPCWithAccounts(t, fee, finalSlot, nil)
}

func budgetBuildRPCWithAccounts(t *testing.T, fee uint64, finalSlot int64, extra []ConfirmedAccount) *RPCClient {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	sf := new(big.Int).Lsh(big.NewInt(1), 60)
	usdc := reserveFixture(t, config.DebtReserve, bridgeUSDC, 42, sf, 1, 1)
	sol := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, 42, new(big.Int).Mul(sf, big.NewInt(100)), 1, 1)
	putKey(t, sol.Data[32:64], budgetSOLMarket)
	binary.LittleEndian.PutUint64(sol.Data[272:280], 9)
	for _, account := range []ConfirmedAccount{usdc, sol} {
		binary.LittleEndian.PutUint64(account.Data[264:272], 1000)
	}
	mint := func(address string, decimals byte) ConfirmedAccount {
		data := make([]byte, 82)
		data[44], data[45] = decimals, 1
		return ConfirmedAccount{Address: address, Owner: classicTokenProgram, Data: data}
	}
	clock := ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Data: make([]byte, 40)}
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	accounts := map[string]ConfirmedAccount{}
	for _, a := range []ConfirmedAccount{usdc, sol, mint(bridgeUSDC, 6), mint(budgetWrappedSOLMint, 9), clock} {
		accounts[a.Address] = a
	}
	for _, a := range extra {
		accounts[a.Address] = a
	}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	reads := 0
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
			reads++
			result = int64(42)
			if reads > 1 {
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
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": fee}
		case "getBlockHeight":
			result = 10 // A signed HOLD at this height is not expired in the DB fixture.
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
				a, ok := accounts[address]
				if !ok {
					t.Fatalf("unexpected valuation account %s", address)
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": values}
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

func TestProductionBridgeRequiresDurableBudgetBeforeSigner(t *testing.T) {
	for _, tc := range []struct {
		action      Action
		amount, fee uint64
	}{
		{ReportNAV, 0, 20_000_000},
		{StageSquadsToVoltr, 1_000_000, 5_000},
		{VoltrRestoreIdle, 1_000_000, 5_000},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			effects, _, _, err := bridgeExpectedEffects(Decision{Action: tc.action, AmountRaw: int64(tc.amount)}, 2_000_000, 1_000_000, 1_000_000)
			if err != nil {
				t.Fatal(err)
			}
			request := bridgeTestRequest(tc.action, tc.amount)
			// Measurement cannot infer pilot authority or authorize signing.
			assertKnownCostExceedsLegacyBudget(t, budgetBuildRPC(t, tc.fee, 42), request, effects)
			err = BuildSimulateAndPersistBridge(context.Background(), &Database{}, budgetBuildRPC(t, tc.fee, 42), "negative-probe", BridgeExecutionEvidence{request, effects})
			if err == nil || err.Error() != "database is not configured" {
				t.Fatalf("unconfigured builder reached signer: %v", err)
			}
		})
	}
}

func TestKnownBuildCostRejectsStaleObservationAndDoesNotGrantAdmission(t *testing.T) {
	request := bridgeTestRequest(ReportNAV, 0)
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := observePhase3KnownBuildCost(context.Background(), budgetBuildRPC(t, 5_000, 42), request, effects)
	if err != nil || cost.TotalMicros <= 0 || cost.TotalMicros >= Phase3TransactionCapMicros || cost.PrincipalMicros != 0 {
		t.Fatalf("unexpected measured report cost: %+v %v", cost, err)
	}
	_, err = observePhase3KnownBuildCost(context.Background(), budgetBuildRPC(t, 5_000, 75), request, effects)
	assertBudgetHold(t, err, "fee_message_or_slot_mismatch")
	// Passing known-cost measurement cannot replace durable admission.
	err = BuildSimulateAndPersistBridge(context.Background(), &Database{}, budgetBuildRPC(t, 5_000, 42), "unreserved", BridgeExecutionEvidence{request, effects})
	if err == nil {
		t.Fatal("unreserved production build passed")
	}
}

func TestProductionKaminoAndJupiterRequireDurableBudgetBeforeSigner(t *testing.T) {
	t.Run("Kamino", func(t *testing.T) {
		request := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegBorrow)
		source, destination := kaminoLegCustodies(kaminoLegBorrow)
		effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
			{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: 2_000_000, AfterRaw: 1_000_000},
			{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: 0, AfterRaw: 1_000_000},
		}}
		assertKnownCostExceedsLegacyBudget(t, budgetBuildRPC(t, 5_000, 42), request, effects)
		err := BuildSimulateAndPersistKamino(context.Background(), &Database{}, budgetBuildRPC(t, 5_000, 42), "negative-kamino", KaminoExecutionEvidence{request, effects})
		if err == nil || err.Error() != "database is not configured" {
			t.Fatalf("unconfigured builder reached signer: %v", err)
		}
	})
	t.Run("Jupiter", func(t *testing.T) {
		request := JupiterSwapRequest{Action: SwapUSDCToPrimeStep, AmountRaw: 1_000_000, QuotedOutputRaw: 990_000, MinimumOutputRaw: 985_050,
			Policy: "FZjjJScy689WWSwhwr2HZPy2aevZukq75niD6gW3b1TG", PolicyAccountDataSHA256: "fdc11ac8e9226feef4db8d30065035fde00d6f2eb9a7f940f6ebffa869962d72",
			Instruction: jupiterTestInstruction(SwapUSDCToPrimeStep, 1_000_000, 990_000, false), RecentBlockhash: bridgeSettings, LastValidBlockHeight: 99}
		minimum := uint64(985_050)
		effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
			{Address: bridgeSquadsATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: 1_000_000, AfterRaw: 0},
			{Address: kaminoPrimeCustody, Owner: classicTokenProgram, Mint: kaminoPrimeMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: minimum, MinimumAfterRaw: &minimum},
		}}
		assertKnownCostExceedsLegacyBudget(t, budgetBuildRPC(t, 5_000, 42), request, effects)
		err := BuildSimulateAndPersistJupiter(context.Background(), &Database{}, budgetBuildRPC(t, 5_000, 42), "negative-jupiter", JupiterExecutionEvidence{request, effects})
		if err == nil || err.Error() != "database is not configured" {
			t.Fatalf("unconfigured builder reached signer: %v", err)
		}
	})
}

func assertKnownCostExceedsLegacyBudget(t *testing.T, rpc *RPCClient, request any, effects ExpectedEffects) {
	t.Helper()
	cost, err := observePhase3KnownBuildCost(context.Background(), rpc, request, effects)
	if err != nil {
		t.Fatal(err)
	}
	b := emptyTestBudget()
	r := testReservation()
	r.UpperMicros = cost.TotalMicros
	assertBudgetHold(t, b.Admit(r), "transaction_cap_exceeded")
}

func TestKnownBuildCostReadsIndependentValuationsTogether(t *testing.T) {
	_, _, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	rpc := budgetBuildRPC(t, 5_000, 42)
	base := rpc.client.Transport
	var started atomic.Int32
	ready := make(chan struct{})
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			return nil, err
		}
		if call.Method == "getFeeForMessage" || call.Method == "getMultipleAccounts" {
			var options struct {
				MinimumSlot int64 `json:"minContextSlot"`
			}
			if err := json.Unmarshal(call.Params[1], &options); err != nil {
				return nil, err
			}
			if options.MinimumSlot != 42 {
				return nil, fmt.Errorf("unexpected minimum slot %d", options.MinimumSlot)
			}
			if started.Add(1) == 3 {
				close(ready)
			}
			select {
			case <-ready:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return base.RoundTrip(request)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cost, err := observePhase3KnownBuildCost(ctx, rpc, evidence.Request, evidence.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if started.Load() != 3 || cost.PrincipalMicros != 100_000 || cost.ValidThroughSlot != 74 {
		t.Fatalf("incomplete concurrent build measurement: reads=%d cost=%+v", started.Load(), cost)
	}
}

func TestKnownBuildCostConcurrentReadsKeepEveryFreshnessBound(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		feeSlot, tokenSlot, solSlot, finalSlot int64
		nullFee                                bool
		hold                                   string
	}{
		{"oldest fee bounds validity", 42, 70, 70, 74, false, ""},
		{"oldest token bounds validity", 70, 42, 70, 74, false, ""},
		{"oldest native bounds validity", 70, 70, 42, 74, false, ""},
		{"future fee rejected", 70, 42, 42, 60, false, "fee_message_or_slot_mismatch"},
		{"future token rejected", 42, 70, 42, 60, false, "missing_stale_or_mismatched_usdc_valuation"},
		{"future native rejected", 42, 42, 70, 60, false, "missing_stale_or_mismatched_usdc_valuation"},
		{"expired fee rejected", 42, 70, 70, 75, false, "fee_message_or_slot_mismatch"},
		{"expired token rejected", 70, 42, 70, 75, false, "missing_stale_or_mismatched_usdc_valuation"},
		{"expired native rejected", 70, 70, 42, 75, false, "missing_stale_or_mismatched_usdc_valuation"},
		{"null fee rejected", 42, 42, 42, 42, true, "network_fee_unavailable"},
		{"token read below floor rejected", 42, 41, 42, 42, false, "build_token_valuation_unavailable"},
		{"native read below floor rejected", 42, 42, 41, 42, false, "build_native_valuation_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
			rpc := budgetBuildRPC(t, 5_000, tc.finalSlot)
			base := rpc.client.Transport
			rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var call struct {
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal(body, &call); err != nil {
					return nil, err
				}
				res, err := base.RoundTrip(request)
				if err != nil {
					return nil, err
				}
				if call.Method != "getFeeForMessage" && call.Method != "getMultipleAccounts" {
					return res, nil
				}
				defer res.Body.Close()
				var payload map[string]any
				if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
					return nil, err
				}
				result := payload["result"].(map[string]any)
				slot := tc.tokenSlot
				if call.Method == "getFeeForMessage" {
					slot = tc.feeSlot
					if tc.nullFee {
						result["value"] = nil
					}
				} else {
					var addresses []string
					if err := json.Unmarshal(call.Params[0], &addresses); err != nil {
						return nil, err
					}
					for _, address := range addresses {
						if address == budgetSOLReserve {
							slot = tc.solSlot
						}
					}
				}
				result["context"].(map[string]any)["slot"] = slot
				encoded, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return response(string(encoded)), nil
			})
			cost, err := observePhase3KnownBuildCost(context.Background(), rpc, evidence.Request, evidence.ExpectedEffects)
			if tc.hold != "" {
				assertBudgetHold(t, err, tc.hold)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cost.ValidThroughSlot != 74 || cost.ObservationSlot != tc.finalSlot || cost.Fee.Slot != tc.feeSlot || cost.TokenPrice == nil || cost.TokenPrice.ObservedSlot != tc.tokenSlot || cost.NativePrice.ObservedSlot != tc.solSlot {
				t.Fatalf("lost independently observed validity: %+v", cost)
			}
		})
	}
}
