package backyardrwa

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
)

// Controlled quote/RPC transport around actual compilers and installed Jupiter
// bytes. Synthetic reserve prices/bridge-policy bytes do not prove live state.
func withdrawalAdmissionFixture(t *testing.T, quoted uint64) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient) {
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
	r, err := manifest.kaminoPacketForRoute(d.Action, kaminoLegWithdraw, 100_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: 1_000_000_000, AfterRaw: 900_000_000},
		{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: 0, AfterRaw: 100_000_000},
	}}
	extra := []ConfirmedAccount{exactReportTicketAccount(t, 1)}
	for i := range manifest.RuntimeBindings.BridgePolicies {
		p := &manifest.RuntimeBindings.BridgePolicies[i]
		data := []byte("controlled-bridge-policy:" + string(p.Action))
		hash := sha256Bytes(data)
		p.DataSHA256 = &hash
		extra = append(extra, ConfirmedAccount{Address: p.Account, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
	}
	binding, err := catalogJupiterBindingForRoute(SwapCollateralToStableStep, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
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
	var installed struct {
		Operations []struct{ PolicyAddress, DataBase64 string }
	}
	read("policy-install-readback-v1.json", &installed)
	for _, p := range installed.Operations {
		if p.PolicyAddress == binding.Policy {
			data, err := base64.StdEncoding.Strict().DecodeString(p.DataBase64)
			if err != nil {
				t.Fatal(err)
			}
			extra = append(extra, ConfirmedAccount{Address: p.PolicyAddress, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
		}
	}
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_000_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 9, 1
	extra = append(extra, reserve, mint)
	rpc := budgetBuildRPCWithAccounts(t, 5_000, 42, extra)
	var headers struct {
		Rows []struct {
			Key         string
			Instruction struct {
				ProgramID, DataBase64 string
				Accounts              []JupiterInstructionAccount
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
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) <= binding.FeeOffset {
		t.Fatal("missing retained exit instruction")
	}
	binary.LittleEndian.PutUint64(data[binding.AmountOffset:], 100_000_000)
	binary.LittleEndian.PutUint64(data[binding.AmountOffset+8:], quoted)
	binary.LittleEndian.PutUint16(data[binding.SlippageOffset:], 50)
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" && req.URL.Path == "/quote" {
			if req.URL.Query().Get("amount") != "100000000" || req.URL.Query().Get("inputMint") != route.Kamino.CollateralMint || req.URL.Query().Get("outputMint") != bridgeUSDC {
				t.Fatal("exit quote changed custody or amount")
			}
			payload = JupiterQuote{InputMint: route.Kamino.CollateralMint, OutputMint: bridgeUSDC, InAmount: "100000000", OutAmount: fmt.Sprint(quoted), OtherAmountThreshold: fmt.Sprint(quoted), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
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
	})})
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
	if !reflect.DeepEqual(actions, []Action{ReportNAV, SwapCollateralToStableStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) ||
		total != plan.ExitAfterMicros || plan.Input.Kind != "kamino" || plan.CurrentCost.PrincipalMicros <= 100_000 || plan.ExitAfterMicros <= 300_000 ||
		plan.QuotedExit == nil || plan.QuotedExit.EstimatedUpperOutputRaw <= plan.QuotedExit.QuotedOutputRaw || plan.QuotedExit.Input.Kind != "jupiter" {
		t.Fatalf("incomplete exit estimate: %+v", plan)
	}
	// Same durable boundary as bridge admission, but missing DB cannot be
	// mistaken for acceptance or bypassed by a successfully constructed quote.
	assertBudgetHold(t, (&Database{}).admitPhase3Withdrawal(context.Background(), rpc, client, manifest, "missing", o, d, evidence), "bridge_admission_database_unavailable")
}

func TestWithdrawalAdmissionRejectsUnsafeOrIncompleteReturn(t *testing.T) {
	for _, mutate := range []func(*Observation, *KaminoExecutionEvidence){
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.PositionDebtRaw = 1 },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.DebtIdleRaw = 1 },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.PositionCollateralRaw++ },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.CollateralIdleRaw = 1 },
	} {
		o, d, evidence, manifest, _, _ := withdrawalAdmissionFixture(t, 100_000)
		mutate(&o, &evidence)
		_, err := observePhase3WithdrawalAdmission(context.Background(), nil, nil, manifest, o, d, evidence)
		assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
	}
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 990_000)
	_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
	assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
	for _, value := range []uint64{0, math.MaxUint64} {
		if _, err = withdrawalUSDCExitEstimate(value); err == nil {
			t.Fatal("invalid quote estimate accepted")
		}
	}
	t.Run("exit policy drift", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		for i := range manifest.RuntimeBindings.BridgePolicies {
			if manifest.RuntimeBindings.BridgePolicies[i].Action == ReportNAV {
				hash := sha256Bytes([]byte("different policy"))
				manifest.RuntimeBindings.BridgePolicies[i].DataSHA256 = &hash
			}
		}
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "withdrawal_exit_policy_drift")
	})
	t.Run("stale construction snapshot", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		o.Snapshot.Slot = 1
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "stale_bridge_admission_snapshot")
	})
	t.Run("unavailable quote", func(t *testing.T) {
		o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
		client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{"error":"no route"}`), nil })
		_, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence)
		assertBudgetHold(t, err, "withdrawal_exit_quote_unavailable")
	})
}

func TestWithdrawalReturnAdmissionContinuesThroughNAVSwapAndBridge(t *testing.T) {
	ctx := context.Background()
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
	plan, err := observePhase3WithdrawalAdmission(ctx, rpc, client, manifest, o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	budget := emptyTestBudget()
	budget.Families["Ethena"] = FamilyBudget{ExitMicros: 1_000_000}
	var spent int64
	consume := func(plan phase3BridgeAdmission) {
		t.Helper()
		request, _, _, err := plan.Input.decode()
		if err != nil {
			t.Fatal(err)
		}
		digest, err := Phase3IntentDigest(request, plan.Input.Effects)
		if err != nil {
			t.Fatal(err)
		}
		r := BudgetReservation{OperationID: "return", Family: "Ethena", IntentSHA256: digest, UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: true}
		if err = budget.Admit(r); err != nil {
			t.Fatal(err)
		}
		if err = budget.Settle(r.OperationID, digest, r.UpperMicros); err != nil {
			t.Fatal(err)
		}
		spent += r.UpperMicros
	}
	consume(plan)
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
	plan, err = observePhase3CollateralReturnAdmission(ctx, rpc, client, manifest, o, d, report, reportEffects)
	if err != nil {
		t.Fatal(err)
	}
	consume(plan)
	swapRequest, swapEffects, _, err := plan.QuotedExit.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	d = Decision{Action: SwapCollateralToStableStep, StrategyKey: o.Snapshot.RouteLane, AmountRaw: 100_000_000}
	plan, err = observePhase3CollateralReturnAdmission(ctx, rpc, nil, manifest, o, d, swapRequest, swapEffects)
	if err != nil {
		t.Fatal(err)
	}
	consume(plan)
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.SquadsIdleRaw = 0, 0, 100_000
	d = Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	reportEffects, _, _, err = bridgeExpectedEffects(d, 0, 0, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	reportEffects.Kind, reportEffects.ReturnData = "bridge", expectedAdaptorReturnData(100_000)
	steps, err := phase3BridgeTemplates(o.Snapshot, d, BridgeExecutionEvidence{report, reportEffects})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		d.Action, d.AmountRaw = step.Request.Action, int64(step.Request.AmountRaw)
		plan, err = observePhase3BridgeAdmission(ctx, rpc, o, d, step)
		if err != nil {
			t.Fatal(err)
		}
		consume(plan)
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
	if budget.Families["Ethena"].SpentMicros != spent || spent <= 400_000 || budget.Families["Ethena"].ExitMicros != 0 ||
		o.Snapshot.VoltrIdleRaw != 100_000 || o.Snapshot.SquadsIdleRaw != 0 || o.Snapshot.VoltrStrategyIdleRaw != 0 {
		t.Fatal("full return lost spent, reserve or custody accounting")
	}
}

func testProductionWithdrawalAdmission(t *testing.T, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
	key := fmt.Sprintf("phase3-withdrawal-producer-%d", time.Now().UnixNano())
	id := key + "-operation"
	b := emptyTestBudget()
	state, err := json.Marshal(map[string]any{"generation": 1, "phase3": b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2::jsonb)`, key, string(state)); err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, manifest.SHA256, *manifest.PolicyCatalog.SHA256)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, d.Action, d.StrategyKey, string(envelope)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "withdrawal-producer", time.Minute); err != nil {
		t.Fatal(err)
	}
	admit := func() error { return db.admitPhase3Withdrawal(ctx, rpc, client, manifest, id, o, d, evidence) }
	assertBudgetHold(t, admit(), "recovery_exceeds_reserved_exit")
	// Controlled historical reserve only. The producer derives the current
	// transaction and remaining exit; it cannot adopt unreserved exposure.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,families,Ethena,exitMicros}','1000000') WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err = admit(); err != nil {
		t.Fatal(err)
	}
	var encoded, budgetBytes []byte
	var hasWire, hasSend bool
	if err = db.pool.QueryRow(ctx, `SELECT operation.expected_effects->'phase3',route.state->'phase3',operation.signed_wire IS NOT NULL,operation.broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE operation_id=$1`, id).Scan(&encoded, &budgetBytes, &hasWire, &hasSend); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encoded, &auth) != nil || json.Unmarshal(budgetBytes, &b) != nil {
		t.Fatal("invalid durable admission")
	}
	r := b.Reservations[id]
	if hasWire || hasSend || auth.BuildInput.Kind != "kamino" || auth.BridgeAdmission == nil || auth.BridgeAdmission.QuotedExit == nil ||
		!r.Recovery || r.ExitBeforeMicros != 1_000_000 || r.ExitAfterMicros != auth.BridgeAdmission.ExitAfterMicros || r.UpperMicros != auth.BridgeAdmission.CurrentCost.TotalMicros {
		t.Fatal("production withdrawal admission did not bind current and future costs")
	}
	if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, evidence.Request, evidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
		t.Fatal(err)
	}
}
