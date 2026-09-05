package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"testing"
)

// Exercise the actual RPC decoder, production compiler and valuation path.
// The transport supplies controlled chain inputs; it rejects every signing,
// simulation and send RPC. This is a local negative witness, not live proof.
func budgetBuildRPC(t *testing.T, fee uint64, finalSlot int64) *RPCClient {
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
			if err != nil || len(decoded) == 0 || decoded[0] != 1 {
				t.Fatal("fee request must contain unsigned one-signer message")
			}
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": fee}
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

func TestProductionBridgeRejectsFreshOverCapCostBeforeSignerOrDatabase(t *testing.T) {
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
			// An uninitialized DB cannot authorize or record a wire. Getting the
			// typed cap HOLD proves this production path rejected before that
			// boundary, signer loading and signed simulation/send.
			err = BuildSimulateAndPersistBridge(context.Background(), &Database{}, budgetBuildRPC(t, tc.fee, 42), "negative-probe", BridgeExecutionEvidence{request, effects})
			assertBudgetHold(t, err, "transaction_cap_exceeded")
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

func TestProductionKaminoAndJupiterRejectFreshOverCapCostBeforeSigner(t *testing.T) {
	t.Run("Kamino", func(t *testing.T) {
		request := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegBorrow)
		source, destination := kaminoLegCustodies(kaminoLegBorrow)
		effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
			{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: 2_000_000, AfterRaw: 1_000_000},
			{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: 0, AfterRaw: 1_000_000},
		}}
		err := BuildSimulateAndPersistKamino(context.Background(), &Database{}, budgetBuildRPC(t, 5_000, 42), "negative-kamino", KaminoExecutionEvidence{request, effects})
		assertBudgetHold(t, err, "transaction_cap_exceeded")
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
		err := BuildSimulateAndPersistJupiter(context.Background(), &Database{}, budgetBuildRPC(t, 5_000, 42), "negative-jupiter", JupiterExecutionEvidence{request, effects})
		assertBudgetHold(t, err, "transaction_cap_exceeded")
	})
}
