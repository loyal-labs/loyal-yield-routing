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
)

// Controlled quote/RPC transport around actual compilers and installed Jupiter
// bytes. Synthetic reserve prices/bridge-policy bytes do not prove live state.
func withdrawalAdmissionFixture(t *testing.T, quoted uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiterClient) {
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
		p.NormalizedDigest = hash
		p.MaskedByteRanges = nil
		p.DataSHA256Raw = hash
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
	rpc := budgetBuildRPCWithAccounts(t, 5_000, 42, append(extra, extraAccounts...))
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
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.DebtIdleRaw = -1 },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.PositionCollateralRaw++ },
		func(o *Observation, _ *KaminoExecutionEvidence) { o.Snapshot.CollateralIdleRaw = 1 },
	} {
		o, d, evidence, manifest, _, _ := withdrawalAdmissionFixture(t, 100_000)
		mutate(&o, &evidence)
		_, err := observePhase3WithdrawalAdmission(context.Background(), nil, nil, manifest, o, d, evidence)
		assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
	}
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 990_000)
	_, err := legacyAdmissionCostCheck(observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, o, d, evidence))
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
				manifest.RuntimeBindings.BridgePolicies[i].NormalizedDigest = hash
				manifest.RuntimeBindings.BridgePolicies[i].DataSHA256Raw = hash
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

// This public finalized account capture is checked in; tests never fetch RPC.
// Its provenance remains in testdata/installed-auto-policy-156.json.
func installedAutoPolicyAccount(t *testing.T) ConfirmedAccount {
	t.Helper()
	raw, err := os.ReadFile("testdata/installed-auto-policy-156.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Address    string `json:"address"`
		DataSHA256 string `json:"dataSha256"`
		Account    struct {
			Data       []string `json:"data"`
			Owner      string   `json:"owner"`
			Executable bool     `json:"executable"`
			Lamports   uint64   `json:"lamports"`
		} `json:"account"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Address != installedAutoPolicyKey || fixture.DataSHA256 != installedAutoPolicyDigest || fixture.Account.Owner != bridgeSquadsProgram || fixture.Account.Executable || fixture.Account.Lamports == 0 || len(fixture.Account.Data) != 2 || fixture.Account.Data[1] != "base64" {
		t.Fatal("installed AUTO policy capture identity drift")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(fixture.Account.Data[0])
	if err != nil || sha256Bytes(data) != installedAutoPolicyDigest {
		t.Fatal("installed AUTO policy capture data drift", err)
	}
	return ConfirmedAccount{Address: fixture.Address, Owner: fixture.Account.Owner, Lamports: fixture.Account.Lamports, Executable: fixture.Account.Executable, Data: data}
}

// Keep positive full-exit admissions on lanes the operator can actually authorize.
// The legacy Ethena fixtures remain the independent pricing/wire tests above.
func supportedFullExitAdmissionFixture(t *testing.T, variant string) (Observation, Decision, KaminoExecutionEvidence, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiterClient) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixtureForLane(t, "OnRe/ONyc/USDC")
	route, err := runtimeRoute(o.Snapshot.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	var kamino KaminoExecutionEvidence
	var swap JupiterExecutionEvidence
	if variant == "auto_funding" {
		m, route, accounts = autoObservationBatch(t, 42, func(batch []ConfirmedAccount) {
			obligation := kaminoObligationImage(t, autoAUTOPYUSD, 42, 100_000_000, 1_000)
			copy(accountAt(batch, autoAUTOPYUSD.Kamino.Obligation).Data, obligation.Data)
			binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.CollateralCustody).Data[64:72], 20_000_000)
			binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.DebtCustody).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeSquadsATA).Data[64:72], 0)
		})
		// Persisted production admission recompiles against the embedded
		// installed binding, not autoObservationBatch's synthetic candidate.
		installed := installedAutoFixtureBinding(t)
		m.RuntimeBindings.AutoPolicy = &installed
		upsertConfirmedAccount(&accounts, installedAutoPolicyAccount(t))
		o, _, err = autoObservationForAccounts(m, 42, accounts)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("auto-collateral-funding-admission"))
		rpc = autoPayoffRPC(t, 42, append(accounts, autoPayoffMints(t, route)...))
		client = autoCandidateJupiter(t, route)
	} else if variant != "payoff" {
		o.Snapshot.SquadsIdleRaw = 0
		binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
		if variant == "funding" {
			o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.CollateralIdleValueRaw = 20_000_000, 20_000_000, 20_000
			binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 20_000_000)
		}
	}
	d := Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, IdempotencyKey: "supported-" + variant}
	switch variant {
	case "payoff":
		d.AmountRaw, d.Reason = 1_000, "withdrawal_repay_debt"
		request, err := m.kaminoPacketForRoute(d.Action, kaminoLegRepay, 1_001, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		request.FullPayoff = true
		request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
		effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1_000, 1_001)
		if err != nil {
			t.Fatal(err)
		}
		kamino = KaminoExecutionEvidence{request, effects}
	case "release":
		d.AmountRaw, d.Reason = 1, "withdrawal_release_repayment_collateral"
		observed, full, err := observeKaminoPayoffWindow(context.Background(), rpc, route, o.Snapshot.Slot, 5)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := decodeKaminoRepaymentRelease(full, route, observed.ObservedSlot)
		if err != nil {
			t.Fatal(err)
		}
		request, err := m.kaminoPacketForRoute(d.Action, kaminoLegWithdraw, bound.ReceiptRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		request.RepaymentRelease = true
		source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
		effects, err := exactKaminoTokenEffects(full, source, destination, bound.LiquidityRaw)
		if err != nil {
			t.Fatal(err)
		}
		kamino = KaminoExecutionEvidence{request, effects}
	case "funding", "auto_funding":
		d.Action, d.AmountRaw, d.Reason = SwapCollateralToDebtStep, o.Snapshot.CollateralIdleRaw, "withdrawal_swap_repayment_buffer"
		swap, err = prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), uint64(debtCashRaw(o.Snapshot)), o.Snapshot.Slot)
		if err != nil {
			t.Fatal(err)
		}
		swap.Request.FullPayoffFunding = true
	default:
		t.Fatal("unsupported full-exit fixture variant", variant)
	}
	return o, d, kamino, swap, m, rpc, client
}

func testProductionWithdrawalAdmission(t *testing.T, url string) {
	for _, variant := range []string{"collateral", "debt_residue", "payoff", "funding", "auto_funding", "release", "entry_swap", "deposit", "borrow", "funding_nav", "leverage", "redeposit"} {
		t.Run(variant, func(t *testing.T) { testProductionWithdrawalAdmissionFixture(t, url, variant) })
	}
	// The legacy catalog's USDC->PYUSD quote is not an edge of the current
	// AUTO combined policy. Refuse it before a build or journal write instead
	// of promoting the old positive fixture to unsupported execution authority.
	t.Run("usdc_funding", func(t *testing.T) {
		ctx, db, key := prepareDebtClearDatabase(t)
		manifest := requireEmbeddedInstalledBinding(t)
		var before, after string
		if err := db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&before); err != nil {
			t.Fatal(err)
		}
		decision := Decision{Action: SwapUSDCToDebtStep, StrategyKey: autoAUTOPYUSD.Lane, AmountRaw: 20_000}
		_, err := prepareJupiterQuoteEvidence(ctx, nil, nil, manifest, decision, 20_000, 0, 42)
		if err == nil || err.Error() != "action SWAP_USDC_TO_DEBT_STEP is not an approved AUTO swap edge" {
			t.Fatalf("unsupported AUTO edge did not fail before quote/RPC access: %v", err)
		}
		var operations int
		if err = db.pool.QueryRow(ctx, `SELECT state::text,(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&after, &operations); err != nil {
			t.Fatal(err)
		}
		if before != after || operations != 0 {
			t.Fatal("unsupported AUTO edge changed authority, budget, or operations")
		}
	})
	t.Run("unsupported_ethena_confirmation", func(t *testing.T) {
		ctx, db, key := prepareDebtClearDatabase(t)
		manifest := requireEmbeddedInstalledBinding(t)
		intent := UnwindIntent{SourceLane: ethenaUSDePYUSD.Lane, Reason: "withdrawal_shortfall", ObservationID: "unsupported-ethena", MaxCollateralRaw: 100_000_000, MaxDebtRaw: 2_000, CostBoundRaw: 1_000_000, BudgetScope: Phase3GoalID, BudgetFamily: "Ethena", EvidenceID: sha256Bytes([]byte("unsupported-ethena")), CreatedAt: time.Now().UTC()}
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
			t.Fatal("unsupported confirmation wrote authority, budget, or operations")
		}
	})
}

func testProductionWithdrawalAdmissionFixture(t *testing.T, url, variant string) {
	debtResidue, payoff, funding, release := variant != "collateral", variant == "payoff", variant == "funding" || variant == "auto_funding", variant == "release"
	entry := variant == "entry_swap"
	deposit := variant == "deposit"
	borrow := variant == "borrow"
	fundingNAV := variant == "funding_nav"
	leverage, redeposit := variant == "leverage", variant == "redeposit"
	if entry || deposit || borrow || leverage || redeposit {
		debtResidue = false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o, d, evidence, manifest, rpc, client := withdrawalAdmissionFixture(t, 100_000)
	if debtResidue {
		o, d, evidence, manifest, rpc, client = debtResidueAdmissionFixture(t, 20_000)
	}
	if deposit {
		o, d, evidence, manifest, rpc, client, _ = depositAdmissionFixture(t, "")
	}
	if borrow {
		o, d, evidence, manifest, rpc, client, _ = borrowAdmissionFixture(t, 20_000, "")
	}
	var fundingEvidence JupiterExecutionEvidence
	if leverage {
		o, d, fundingEvidence, manifest, rpc, client, _ = leverageAdmissionFixture(t, 20_000, "")
	}
	if redeposit {
		o, d, evidence, manifest, rpc, client, _ = depositAdmissionFixtureForPosition(t, "", true)
	}
	if entry {
		o, d, fundingEvidence, manifest, rpc, client, _ = entrySwapAdmissionFixture(t)
	}
	confirmedExit := payoff || funding || release
	if confirmedExit {
		o, d, evidence, fundingEvidence, manifest, rpc, client = supportedFullExitAdmissionFixture(t, variant)
		debtResidue = o.Snapshot.RouteLane == autoAUTOPYUSD.Lane
	}
	var navEvidence BridgeExecutionEvidence
	if fundingNAV {
		o, d, navEvidence, manifest, rpc, client, _ = fundingContinuationFixture(t, 20_000)
	}
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
	admit := func() error {
		if leverage {
			return db.admitPhase3LeverageSwap(ctx, rpc, client, manifest, id, o, d, fundingEvidence)
		}
		if redeposit {
			return db.admitPhase3Deposit(ctx, rpc, client, manifest, id, o, d, evidence)
		}
		if fundingNAV {
			return db.admitPhase3Funding(ctx, rpc, client, manifest, id, o, d, navEvidence.Request, navEvidence.ExpectedEffects)
		}
		if borrow {
			return db.admitPhase3Borrow(ctx, rpc, client, manifest, id, o, d, evidence)
		}
		if deposit {
			return db.admitPhase3Deposit(ctx, rpc, client, manifest, id, o, d, evidence)
		}
		if entry {
			return db.admitPhase3EntrySwap(ctx, rpc, client, manifest, id, o, d, fundingEvidence)
		}
		if funding {
			return db.admitPhase3Funding(ctx, rpc, client, manifest, id, o, d, fundingEvidence.Request, fundingEvidence.ExpectedEffects)
		}
		return db.admitPhase3Withdrawal(ctx, rpc, client, manifest, id, o, d, evidence)
	}
	beforeReserve, beforeJSON := int64(1_000_000), "1000000"
	if variant == "auto_funding" {
		// This complete non-USDC return measures 1,353,645 micros across all
		// legs. The test operator explicitly approves $1.50 from existing
		// reserves; per-transaction, family and goal caps remain unchanged.
		beforeReserve, beforeJSON = 1_500_000, "1500000"
	}
	family := phase3BudgetFamilyForLane(o.Snapshot.RouteLane)
	var confirmation DebtClearConfirmation
	var intent UnwindIntent
	if confirmedExit {
		assertBudgetHold(t, admit(), "debt_clear_confirmation_required")
		var admitted, wired, sent, authorized bool
		if err = db.pool.QueryRow(ctx, `SELECT o.expected_effects ? 'phase3',o.signed_wire IS NOT NULL,o.broadcast_intent_at IS NOT NULL,s.state ? 'debtClearAuthority' FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE operation_id=$1`, id).Scan(&admitted, &wired, &sent, &authorized); err != nil {
			t.Fatal(err)
		}
		if admitted || wired || sent || authorized {
			t.Fatal("unconfirmed exit wrote admission, authority, or wire")
		}
		if err = db.MarkPreBroadcastFailed(ctx, id, Decided, "fixture_unconfirmed_exit"); err != nil {
			t.Fatal(err)
		}
		confirmation = DebtClearConfirmation{RequestID: sha256Bytes([]byte(key)), ConfirmedBy: "privileged-test-operator", ConfirmationRecord: sha256Bytes([]byte(key + "-confirmation")), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
		intent = UnwindIntent{SourceLane: o.Snapshot.RouteLane, Reason: "withdrawal_shortfall", ObservationID: o.Snapshot.ObservationID, MaxCollateralRaw: o.Snapshot.PositionCollateralRaw, MaxDebtRaw: 2_000, CostBoundRaw: beforeReserve, BudgetScope: Phase3GoalID, BudgetFamily: family, EvidenceID: sha256Bytes([]byte(key + "-observed-exit")), CreatedAt: time.Now().UTC()}
		assertBudgetHold(t, db.commitUnwindIntentWithConfirmation(ctx, key, &intent, manifest, confirmation), "unwind_requires_existing_exit_reservation")
	} else if leverage || redeposit {
		reason := "leverage_requires_reserved_position"
		if redeposit {
			reason = "redeposit_requires_reserved_position"
		}
		assertBudgetHold(t, admit(), reason)
		beforeReserve, beforeJSON = 10_000, "10000"
	} else if borrow {
		assertBudgetHold(t, admit(), "borrow_requires_reserved_position")
		beforeReserve, beforeJSON = 10_000, "10000"
	} else if deposit {
		assertBudgetHold(t, admit(), "deposit_requires_reserved_collateral_custody")
		beforeReserve, beforeJSON = 10_000, "10000"
	} else if entry {
		assertBudgetHold(t, admit(), "entry_requires_reserved_bridge_custody")
		beforeReserve, beforeJSON = 10_000, "10000"
	} else {
		assertBudgetHold(t, admit(), "recovery_exceeds_reserved_exit")
	}
	// Controlled historical reserve only. The producer derives the current
	// transaction and remaining exit; it cannot adopt unreserved exposure.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,ARRAY['phase3','families',$3,'exitMicros'],$2::jsonb) WHERE route_key=$1`, key, beforeJSON, family); err != nil {
		t.Fatal(err)
	}
	if confirmedExit {
		if err = db.commitUnwindIntentWithConfirmation(ctx, key, &intent, manifest, confirmation); err != nil {
			t.Fatal(err)
		}
		id += "-confirmed"
		insertDebtClearOperation(t, ctx, db, key, id, o, d, manifest)
	}
	if err = admit(); err != nil {
		if variant == "auto_funding" {
			plan, planErr := observePhase3FundingAdmission(ctx, rpc, client, manifest, o, d, fundingEvidence.Request, fundingEvidence.ExpectedEffects)
			var bound, used int64
			readErr := db.pool.QueryRow(ctx, `SELECT (state->'debtClearAuthority'->'origin'->>'costBoundRaw')::bigint,(state->'debtClearAuthority'->>'usedCostMicros')::bigint FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&bound, &used)
			t.Fatalf("AUTO funding admission: %v; planError=%v current=%d tail=%d authorityBound=%d used=%d readError=%v", err, planErr, plan.CurrentCost.TotalMicros, plan.ExitAfterMicros, bound, used, readErr)
		}
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
	if r.Family != family || (confirmedExit && (auth.DebtClear == nil || auth.DebtClear.ID != confirmation.RequestID || auth.DebtClear.FirstOperationID != id)) {
		t.Fatal("admission lost its scoped confirmation or budget family")
	}
	kind := "kamino"
	if fundingNAV {
		kind = "bridge"
	}
	if funding || entry || leverage {
		kind = "jupiter"
	}
	if auth.BridgeAdmission == nil {
		t.Fatal("production withdrawal admission omitted the admission plan")
	}
	wantRecovery := !(entry || deposit || borrow || leverage || redeposit)
	wantExitAfter := auth.BridgeAdmission.ExitAfterMicros
	if wantRecovery && wantExitAfter > 0 {
		// A nonterminal exit keeps unspent historical headroom, not just the
		// freshly measured tail. A terminal zero tail still clears the reserve.
		wantExitAfter = max(wantExitAfter, beforeReserve-auth.BridgeAdmission.CurrentCost.TotalMicros)
	}
	if hasWire || hasSend || auth.BuildInput.Kind != kind || auth.BridgeAdmission.QuotedExit == nil ||
		r.Recovery != wantRecovery || r.ExitBeforeMicros != beforeReserve || r.ExitAfterMicros != wantExitAfter || r.UpperMicros != auth.BridgeAdmission.CurrentCost.TotalMicros {
		t.Fatalf("production withdrawal admission did not bind current and future costs: wire=%t send=%t kind=%q wantKind=%q quotedExit=%t recovery=%t wantRecovery=%t before=%d wantBefore=%d reservedAfter=%d wantAfter=%d measuredAfter=%d upper=%d currentCost=%d",
			hasWire, hasSend, auth.BuildInput.Kind, kind, auth.BridgeAdmission.QuotedExit != nil,
			r.Recovery, wantRecovery, r.ExitBeforeMicros, beforeReserve,
			r.ExitAfterMicros, wantExitAfter, auth.BridgeAdmission.ExitAfterMicros, r.UpperMicros, auth.BridgeAdmission.CurrentCost.TotalMicros)
	}
	exitCount := 9
	if payoff {
		exitCount = 11
	}
	if funding {
		exitCount = 13
	}
	if release {
		exitCount = 15
	}
	if fundingNAV {
		exitCount = 16
	}
	if leverage || redeposit {
		if len(auth.BridgeAdmission.Exit) != 17 || auth.BridgeAdmission.Payoff == nil || auth.BridgeAdmission.FundingRelease == nil || auth.BridgeAdmission.PayoffWithdrawal == nil || auth.BridgeAdmission.ExitAfterMicros <= beforeReserve {
			t.Fatal("durable position entry omitted complete return")
		}
		if leverage {
			if auth.BridgeAdmission.LeverageProjection == nil {
				t.Fatal("missing swap poststate")
			}
			if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, fundingEvidence.Request, fundingEvidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
				t.Fatal(err)
			}
		} else {
			if auth.BridgeAdmission.DepositProjection == nil {
				t.Fatal("missing redeposit poststate")
			}
			if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, evidence.Request, evidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	if debtResidue && (len(auth.BridgeAdmission.AdditionalQuotedExits) != 1 || len(auth.BridgeAdmission.Exit) != exitCount) {
		t.Fatal("durable reservation dropped debt conversion")
	}
	if confirmedExit && !debtResidue {
		if len(auth.BridgeAdmission.AdditionalQuotedExits) != 0 || len(auth.BridgeAdmission.Exit) != exitCount-2 {
			t.Fatal("USDC full exit omitted its return or added a debt conversion")
		}
		for _, step := range auth.BridgeAdmission.Exit {
			if step.Action == SwapDebtToUSDCStep || step.Action == SwapUSDCToDebtStep {
				t.Fatal("USDC full exit included a self-swap")
			}
		}
	}
	if payoff && (auth.BridgeAdmission.Payoff == nil || auth.BridgeAdmission.Payoff.UpperDebtRaw != 1_001 || auth.BridgeAdmission.PayoffWithdrawal == nil) {
		t.Fatal("durable funded payoff omitted interest bound or full withdrawal")
	}
	if deposit && (auth.BridgeAdmission.DepositProjection == nil || auth.BridgeAdmission.PayoffWithdrawal == nil || len(auth.BridgeAdmission.Exit) != 9 || auth.BridgeAdmission.ExitAfterMicros <= beforeReserve) {
		t.Fatal("durable deposit omitted simulated receipts or full return reserve")
	}
	if borrow && (auth.BridgeAdmission.BorrowProjection == nil || auth.BridgeAdmission.Payoff == nil || auth.BridgeAdmission.Payoff.ThroughUnix != 1420 || auth.BridgeAdmission.BorrowRelease == nil || auth.BridgeAdmission.PayoffRepayment == nil || auth.BridgeAdmission.PayoffWithdrawal == nil || len(auth.BridgeAdmission.Exit) != 17 || auth.BridgeAdmission.ExitAfterMicros <= beforeReserve) {
		t.Fatal("durable borrowing omitted projected state or full fee/interest return")
	}
	if entry {
		if len(auth.BridgeAdmission.Exit) != 7 || auth.BridgeAdmission.ExitAfterMicros <= beforeReserve {
			t.Fatal("entry did not extend its full return reservation")
		}
		if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, fundingEvidence.Request, fundingEvidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
			t.Fatal(err)
		}
		return
	}
	if fundingNAV {
		if auth.BridgeAdmission.FundingRelease == nil || auth.BridgeAdmission.FundingSwap == nil || auth.BridgeAdmission.Payoff == nil || auth.BridgeAdmission.Payoff.ThroughUnix != 1360 {
			t.Fatal("durable NAV omitted release and combined funding")
		}
		if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, navEvidence.Request, navEvidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
			t.Fatal(err)
		}
		return
	}
	if release && (auth.BridgeAdmission.PayoffRepayment == nil || auth.BridgeAdmission.FundingSwap == nil || auth.BridgeAdmission.Payoff.ThroughUnix != auth.BridgeAdmission.Payoff.ChainUnix+5*kaminoPayoffWindowSeconds) {
		t.Fatal("durable release omitted full funding/return or interest horizon")
	}
	if funding {
		if auth.BridgeAdmission.PayoffRepayment == nil || auth.BridgeAdmission.FundingSwap == nil || auth.BridgeAdmission.Payoff.ThroughUnix != auth.BridgeAdmission.Payoff.ChainUnix+3*kaminoPayoffWindowSeconds {
			t.Fatal("durable funding omitted full interest horizon or return template")
		}
		request, _, _, err := auth.BuildInput.decodeWithManifest(manifest)
		if err != nil || request.(JupiterSwapRequest).Action != d.Action {
			t.Fatal("durable funding changed the selected source asset", err)
		}
		if err = manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, fundingEvidence.Request, fundingEvidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, evidence.Request, evidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
		t.Fatal(err)
	}
}
