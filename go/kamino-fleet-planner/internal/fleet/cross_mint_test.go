package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func testPubkey(seed byte) string { return encodeBase58(bytes.Repeat([]byte{seed}, 32)) }
func rawIX(ix RouteInstruction) rawJupiterInstruction {
	accounts := make([]rawJupiterAccount, len(ix.Accounts))
	for i, a := range ix.Accounts {
		accounts[i] = rawJupiterAccount{a.Address, a.Signer, a.Writable}
	}
	return rawJupiterInstruction{ix.Program, accounts, base64.StdEncoding.EncodeToString(ix.Data)}
}

func validJupiterBuildFixture(t *testing.T) ([]byte, crossMintPlan) {
	t.Helper()
	vault := testPubkey(31)
	inputATA, err := deriveATA(vault, USDCMint, tokenProgram)
	if err != nil {
		t.Fatal(err)
	}
	outputATA, err := deriveATA(vault, USDTMint, tokenProgram)
	if err != nil {
		t.Fatal(err)
	}
	amount, quoted, slippage := uint64(1_000_000), uint64(999_000), uint16(10)
	data := append([]byte{}, jupiterRouteV2Discriminator...)
	data = appendU64x(data, amount)
	data = appendU64x(data, quoted)
	data = append(data, byte(slippage), byte(slippage>>8), 0)
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], 1)
	data = append(data, count[:]...)
	binary.BigEndian.PutUint32(count[:], 104)
	data = append(data, count[:]...)
	data = append(data, 1, 0x10, 0x27, 0, 1)
	amm, state, poolA, poolB := testPubkey(32), testPubkey(33), testPubkey(34), testPubkey(35)
	accounts := []InstructionAccount{{vault, true, false}, {inputATA, false, true}, {outputATA, false, true}, {USDCMint, false, false}, {USDTMint, false, false}, {tokenProgram, false, false}, {tokenProgram, false, false}, {jupiterProgram, false, false}, {jupiterEvent, false, false}, {jupiterProgram, false, false}, {alphaQProgram, false, false}, {vault, false, false}, {amm, false, false}, {state, false, true}, {inputATA, false, true}, {outputATA, false, true}, {poolA, false, true}, {poolB, false, true}, {poolA, false, false}, {poolB, false, false}, {poolB, false, true}, {tokenProgram, false, false}, {instructionsSysvar, false, false}, {jupiterProgram, false, false}}
	swap := RouteInstruction{"jupiter_exact_in", jupiterProgram, accounts, data}
	percent := 100.0
	route := rawJupiterRoute{rawJupiterSwapInfo{amm, "AlphaQ", USDCMint, USDTMint, strconv.FormatUint(amount, 10), strconv.FormatUint(quoted, 10)}, &percent, 10_000}
	price := append([]byte{3}, make([]byte, 8)...)
	raw := rawJupiterBuild{InputMint: USDCMint, OutputMint: USDTMint, InAmount: strconv.FormatUint(amount, 10), OutAmount: strconv.FormatUint(quoted, 10), OtherAmountThreshold: strconv.FormatUint(thresholdFor(quoted, slippage), 10), SwapMode: "ExactIn", SlippageBPS: slippage, RoutePlan: []rawJupiterRoute{route}, ComputeBudgetInstructions: []rawJupiterInstruction{{ProgramID: computeProgram, Data: base64.StdEncoding.EncodeToString(price), Accounts: []rawJupiterAccount{}}}, SetupInstructions: []rawJupiterInstruction{}, SwapInstruction: rawIX(swap), OtherInstructions: []rawJupiterInstruction{}, AddressesByLookupTableAddress: map[string][]string{}, BlockhashWithMetadata: rawJupiterBlockhash{bytes.Repeat([]byte{7}, 32), 12345, json.RawMessage(`"2026-01-01T00:00:00Z"`)}}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return body, crossMintPlan{Kind: "cross_mint_jupiter", SourceMint: USDCMint, TargetMint: USDTMint, Amount: amount}
}

func TestValidateJupiterEnvelopeAcceptsStrictOneHopAlphaQ(t *testing.T) {
	body, plan := validJupiterBuildFixture(t)
	validated, err := validateJupiterEnvelope(body, plan, testPubkey(31), 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Dialect != "route_v2" || validated.RouteSteps != 1 || validated.MinimumOutput != thresholdFor(999_000, 10) {
		t.Fatalf("unexpected validation: %+v", validated)
	}
}

func TestValidateJupiterEnvelopeRejectsThresholdAndResidualTampering(t *testing.T) {
	body, plan := validJupiterBuildFixture(t)
	var raw rawJupiterBuild
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	raw.OtherAmountThreshold = "1"
	tampered, _ := json.Marshal(raw)
	if _, err := validateJupiterEnvelope(tampered, plan, testPubkey(31), 50, nil); err == nil {
		t.Fatal("accepted invalid minimum threshold")
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	raw.SwapInstruction.Accounts[13].Pubkey = jupiterProgram
	tampered, _ = json.Marshal(raw)
	if _, err := validateJupiterEnvelope(tampered, plan, testPubkey(31), 50, nil); err == nil {
		t.Fatal("accepted protected account as AlphaQ state")
	}
}

func TestCrossMintEconomicThresholdsRoundAndFenceProfitability(t *testing.T) {
	minimum, err := minimumEconomicOutput(1_000_001, 50)
	if err != nil || minimum != 995_001 {
		t.Fatalf("minimum=%d err=%v", minimum, err)
	}
	plan := json.RawMessage(`{"holding_horizon_seconds":31536000,"estimated_execution_costs":{"kind":"cross_mint_jupiter","jupiter_swap_usd_micros":100,"deposit_usd_micros":100}}`)
	profitable, err := minimumProfitableCrossMintOutput(plan, 1_000_000, 100, 500)
	if err != nil {
		t.Fatal(err)
	}
	if profitable >= 1_000_000 || profitable == 0 {
		t.Fatalf("unexpected profitable threshold %d", profitable)
	}
}

func TestJupiterFetchUsesNarrowDirectAlphaQContract(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("inputMint") != USDCMint || q.Get("outputMint") != USDTMint || q.Get("amount") != "42" || q.Get("taker") != testPubkey(31) || q.Get("slippageBps") != "50" || q.Get("maxAccounts") != "48" || q.Get("onlyDirectRoutes") != "true" || q.Get("dexes") != "AlphaQ" || r.Header.Get("x-api-key") != "key" {
			t.Errorf("unexpected request: %s headers=%v", r.URL.RawQuery, r.Header)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewJupiterBuildClient(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	client.client = server.Client()
	if _, err = client.fetch(context.Background(), USDCMint, USDTMint, 42, testPubkey(31), 50); err != nil {
		t.Fatal(err)
	}
}

func TestToken2022RejectsUnsupportedAndActiveExtensions(t *testing.T) {
	base := make([]byte, 166)
	base[165] = 1
	unsupported := append(append([]byte{}, base...), 20, 0, 0, 0)
	if err := validateToken2022Extensions(unsupported, 1); err == nil {
		t.Fatal("accepted unsupported mint extension")
	}
	hook := append(append([]byte{}, base...), 14, 0, 64, 0)
	hook = append(hook, make([]byte, 64)...)
	hook[len(hook)-1] = 1
	if err := validateToken2022Extensions(hook, 1); err == nil {
		t.Fatal("accepted active transfer hook")
	}
}
