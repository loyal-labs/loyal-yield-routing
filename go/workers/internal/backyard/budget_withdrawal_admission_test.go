package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Controlled quote/RPC transport around actual compilers and installed Jupiter
// bytes. Synthetic reserve prices/bridge-policy bytes do not prove live state.
func withdrawalAdmissionFixture(t *testing.T, quoted uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	route := ethenaUSDePYUSD
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	o := tickObservation(Snapshot{ObservationID: "withdrawal-admission", Slot: 42, Fresh: true, RouteKind: RouteKind,
		RouteLane: route.Lane, StrategyKey: route.Lane, HasPosition: true, PositionCollateralRaw: 100_000_000,
		PositionCollateralValueRaw: 100_000, ReportSnapshotDigest: sha256Bytes([]byte("controlled-withdrawal-state"))})
	d := Decision{Action: DeleverRouteStep, AmountRaw: 0, StrategyKey: route.Lane, Reason: "withdrawal_withdraw_collateral", IdempotencyKey: "withdrawal-admission"}
	r, err := manifest.kaminoPacketForRoute(testPolicies(t), d.Action, kaminoLegWithdraw, 100_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: 1_000_000_000, AfterRaw: 900_000_000},
		{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: 0, AfterRaw: 100_000_000},
	}}
	o.policies = testPolicies(t)
	extra := []ConfirmedAccount{exactReportTicketAccount(t, 1)}
	read := func(path string, out any) {
		t.Helper()
		data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_000_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 9, 1
	extra = append(extra, reserve, mint)
	rpc := budgetBuildRPCWithAccounts(t, 5_000, 42, append(extra, extraAccounts...))
	var headers struct {
		Rows []struct {
			Key         string
			Instruction struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	read("policy-jupiter-headers-v1.json", &headers)
	var instruction JupiterSwapInstruction
	for _, row := range headers.Rows {
		if row.Key == "USDe->USDC" {
			instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
		}
	}
	edges, leg, err := catalogEdge(SwapCollateralToStableStep, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	binding := edges[leg]
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) <= binding.feeAt() {
		t.Fatal("missing retained exit instruction")
	}
	binary.LittleEndian.PutUint64(data[binding.amountAt():], 100_000_000)
	binary.LittleEndian.PutUint64(data[binding.amountAt()+8:], quoted)
	binary.LittleEndian.PutUint16(data[binding.slippageAt():], 50)
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" && req.URL.Path == "/quote" {
			if req.URL.Query().Get("amount") != "100000000" || req.URL.Query().Get("inputMint") != route.Kamino.CollateralMint || req.URL.Query().Get("outputMint") != bridgeUSDC {
				t.Fatal("exit quote changed custody or amount")
			}
			payload = jupiter.Quote{InputMint: route.Kamino.CollateralMint, OutputMint: bridgeUSDC, InAmount: "100000000", OutAmount: fmt.Sprint(quoted), OtherAmountThreshold: fmt.Sprint(quoted), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else if req.Method == "POST" && req.URL.Path == "/swap-instructions" {
			payload = map[string]any{"swapInstruction": instruction}
		} else {
			t.Fatal("unexpected quote transport request")
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return response(string(encoded)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return o, d, KaminoExecutionEvidence{r, effects}, manifest, rpc, client
}

func TestWithdrawalAdmissionPricesCompleteCrossProtocolReturn(t *testing.T) {
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
	plan, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	quotes := planSwapQuotes(t, plan)
	if !reflect.DeepEqual(actions, []Action{ReportNAV, SwapCollateralToStableStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) ||
		total != plan.ExitAfterMicros || plan.Input.Kind != "kamino" || plan.CurrentCost.PrincipalMicros <= 100_000 || plan.ExitAfterMicros <= 300_000 || len(quotes) != 1 {
		t.Fatalf("incomplete exit estimate: %+v", plan)
	}
	if upper, err := withdrawalUSDCExitEstimate(quotes[0].QuotedOutputRaw); err != nil || upper <= quotes[0].QuotedOutputRaw {
		t.Fatalf("exit estimate is not margined above the quote: %+v %v", quotes[0], err)
	}
}

func TestWithdrawalPricingRejectsUnsafeOrIncompleteReturn(t *testing.T) {
	for _, mutate := range []func(*Observation, *KaminoExecutionEvidence){
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.PositionDebtRaw = 1 },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.DebtIdleRaw = -1 },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.PositionCollateralRaw++ },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.CollateralIdleRaw = 1 },
	} {
		o, d, evidence, manifest, _, _ := withdrawalAdmissionFixture(t, 100_000)
		mutate(&o, &evidence)
		_, err := observePhase3WithdrawalAdmission(context.Background(), nil, nil, manifest, o, d, evidence)
		assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
	}
	for _, value := range []uint64{0, math.MaxUint64} {
		if _, err := withdrawalUSDCExitEstimate(value); err == nil {
			t.Fatal("invalid quote estimate accepted")
		}
	}
	t.Run("exit policy not installed", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		o.policies = installedPolicies{}
		for key, view := range testPolicies(t) {
			if key != (policyKey{action: ReportNAV}) {
				o.policies[key] = view
			}
		}
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "REPORT_NAV policy not installed")
	})
	t.Run("stale construction snapshot", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		o.Snapshot.Slot = 1
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "stale_bridge_admission_snapshot")
	})
	t.Run("unavailable quote", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		fixtureHTTP(client).Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{"error":"no route"}`), nil })
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "withdrawal_exit_quote_unavailable")
	})
}

func TestWithdrawalReturnContinuesThroughNAVSwapAndBridge(t *testing.T) {
	ctx := context.Background()
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
	if _, err := observePhase3WithdrawalAdmission(ctx, rpc, client, manifest, o, d, evidence); err != nil {
		t.Fatal(err)
	}
	// Controlled poststates are bookkeeping witnesses, not claims that any
	// transaction ran. Each subsequent price uses the actual production path.
	o.Snapshot.HasPosition = false
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 0, 0
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 100_000_000, 100_000_000
	report := bridgeTestRequest(ReportNAV, 0)
	report.Report.Sequence, report.Report.ObservedSlot, report.Report.NAVAfterRaw = 42, 42, 100_000
	reportEffects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	reportEffects.Kind, reportEffects.ReturnData = "bridge", expectedAdaptorReturnData(100_000)
	d = Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	plan, err := observePhase3CollateralReturnAdmission(ctx, rpc, client, manifest, o, d, report, reportEffects)
	if err != nil {
		t.Fatal(err)
	}
	var swapTemplate *phase3BuildInput
	for _, step := range plan.Exit {
		if step.Action == SwapCollateralToStableStep {
			swapTemplate = step.Template
		}
	}
	if swapTemplate == nil {
		t.Fatal("NAV omitted the collateral conversion")
	}
	swapRequest, swapEffects, _, err := swapTemplate.decode()
	if err != nil {
		t.Fatal(err)
	}
	d = Decision{Action: SwapCollateralToStableStep, StrategyKey: o.Snapshot.RouteLane, AmountRaw: 100_000_000}
	if _, err = observePhase3CollateralReturnAdmission(ctx, rpc, nil, manifest, o, d, swapRequest, swapEffects); err != nil {
		t.Fatal(err)
	}
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.SquadsIdleRaw = 0, 0, 100_000
	d = Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	reportEffects, _, _, err = bridgeExpectedEffects(d, 0, 0, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	reportEffects.Kind, reportEffects.ReturnData = "bridge", expectedAdaptorReturnData(100_000)
	steps, err := phase3BridgeTemplates(testPolicies(t), o.Snapshot, d, BridgeExecutionEvidence{report, reportEffects})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		d.Action, d.AmountRaw = step.Request.Action, int64(step.Request.AmountRaw)
		if _, err = observePhase3BridgeAdmission(ctx, rpc, o, d, step); err != nil {
			t.Fatal(err)
		}
		for _, effect := range step.ExpectedEffects.Accounts {
			switch effect.Address {
			case bridgeIdleATA:
				o.Snapshot.VoltrIdleRaw = int64(effect.AfterRaw)
			case bridgeStrategyATA:
				o.Snapshot.VoltrStrategyIdleRaw = int64(effect.AfterRaw)
			case bridgeSquadsATA:
				o.Snapshot.SquadsIdleRaw = int64(effect.AfterRaw)
			}
		}
	}
	if o.Snapshot.VoltrIdleRaw != 100_000 || o.Snapshot.SquadsIdleRaw != 0 || o.Snapshot.VoltrStrategyIdleRaw != 0 {
		t.Fatal("full return lost custody")
	}
}

// Unsupported exits refuse before any quote, journal or authority write: the
// legacy catalog's USDC->PYUSD quote is not an edge of the current AUTO
// combined policy, and an Ethena unwind is outside the installed lane authority.
func TestUnsupportedExitsRefuseBeforeAnyWrite(t *testing.T) {
	t.Run("usdc_funding", func(t *testing.T) {
		ctx, db, key := prepareDebtClearDatabase(t)
		manifest := embeddedTestManifest(t)
		var before, after string
		if err := db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&before); err != nil {
			t.Fatal(err)
		}
		decision := Decision{Action: SwapUSDCToDebtStep, StrategyKey: autoAUTOPYUSD.Lane, AmountRaw: 20_000}
		_, err := prepareJupiterQuoteEvidence(ctx, nil, nil, manifest, testPolicies(t), decision, 20_000, 0, 42)
		if err == nil || err.Error() != "action SWAP_USDC_TO_DEBT_STEP is not an approved AUTO Jupiter edge" {
			t.Fatalf("unsupported AUTO edge did not fail before quote/RPC access: %v", err)
		}
		var operations int
		if err = db.pool.QueryRow(ctx, `SELECT state::text,(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&after, &operations); err != nil {
			t.Fatal(err)
		}
		if before != after || operations != 0 {
			t.Fatal("unsupported AUTO edge changed authority or operations")
		}
	})
	t.Run("unsupported_ethena_confirmation", func(t *testing.T) {
		ctx, db, key := prepareDebtClearDatabase(t)
		manifest := embeddedTestManifest(t)
		intent := UnwindIntent{SourceLane: ethenaUSDePYUSD.Lane, Reason: "withdrawal_shortfall", ObservationID: "unsupported-ethena", MaxCollateralRaw: 100_000_000, MaxDebtRaw: 2_000, EvidenceID: sha256Bytes([]byte("unsupported-ethena")), CreatedAt: time.Now().UTC()}
		confirmation := DebtClearConfirmation{RequestID: sha256Bytes([]byte(key)), ConfirmedBy: "privileged-test-operator", ConfirmationRecord: sha256Bytes([]byte(key + "-confirmation")), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
		var before, after string
		if err := db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := db.commitUnwindIntentWithConfirmation(ctx, key, &intent, manifest, confirmation); err == nil || err.Error() != "invalid_unwind_intent" {
			t.Fatalf("unsupported Ethena confirmation did not fail at lane authority: %v", err)
		}
		var operations int
		if err := db.pool.QueryRow(ctx, `SELECT state::text,(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&after, &operations); err != nil {
			t.Fatal(err)
		}
		if before != after || operations != 0 {
			t.Fatal("unsupported confirmation wrote authority or operations")
		}
	})
}
