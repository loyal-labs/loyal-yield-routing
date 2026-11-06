package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func executionFixture(t *testing.T, v *Vault) (Observation, Decision) {
	t.Helper()
	_, _, _, accounts := selectorDestinationFixture(t)
	s := Snapshot{ObservationID: "execution", RouteKind: RouteKind, RouteLane: MapleSyrupUSDCLaneID,
		Slot: 42, Fresh: true, Target: LaneTarget{Rule: ruleWithdrawal, Outbound: true},
		WithdrawalDemandRaw: 5_000_000_000, TotalVaultNAVRaw: 10_000_000_000, StrategyNAVRaw: 10_000_000_000,
		CollateralIdleRaw: 5_000_000_000, CollateralIdleValueRaw: 5_000_000_000}
	s.net = fixtureSnapshotCertificate(s)
	s.net.vault, s.net.manifest = v.squads, v.manifest.SHA256
	o := Observation{ObservedAt: time.Now(), Snapshot: s, routeBatch: &routeObservationBatch{
		Slot: s.Slot, ObservationID: s.ObservationID, ManifestSHA256: v.manifest.SHA256, Accounts: accounts}}
	return o, Decision{Action: SwapCollateralToStableStep, Reason: outboundCollateralReason, AmountRaw: 5_000_000_000,
		StrategyKey: s.RouteLane, IdempotencyKey: "execution", MaxSlippageBPS: 20}
}

func TestExecutionAtomicPortionsKeepWithdrawSwapRepayTogether(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{62}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, original := range []*Vault{rwa, looping} {
		v := *original
		v.ProtectExecution, v.manifest.vault = true, &v
		for _, kind := range []StepKind{stepUp, stepDown, stepExit} {
			for _, held := range []uint64{0, 2_000_000_000} {
				if kind == stepUp && held != 0 {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/held=%d", v.Name, kind, held), func(t *testing.T) {
					o, d := executionFixture(t, &v)
					route, _ := v.route(d.StrategyKey)
					reserve, err := decodeKaminoReserve(accountAt(o.routeBatch.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
					if err != nil {
						t.Fatal(err)
					}
					policies, _, _ := stepTestPolicies(t)
					rpc := budgetBuildRPCWithAccounts(t, 5000, o.Snapshot.Slot, o.routeBatch.Accounts)
					quotes := 0
					client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
						var payload any
						switch req.URL.Path {
						case "/quote":
							quotes++
							q := req.URL.Query()
							amount, _ := strconv.ParseUint(q.Get("amount"), 10, 64)
							if amount > uint64(executionTradeValueRaw) {
								t.Errorf("oversized quote: %d", amount)
							}
							payload = jupiter.Quote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(amount), OtherAmountThreshold: fmt.Sprint(jupiterOutputFloor(amount, 20)), SwapMode: "ExactIn", SlippageBPS: 20, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
						case "/swap-instructions":
							var body struct{ QuoteResponse jupiter.Quote }
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							amount, _ := strconv.ParseUint(body.QuoteResponse.InAmount, 10, 64)
							action := SwapCollateralToStableStep
							if body.QuoteResponse.InputMint == fleet.USDCMint {
								action = SwapStableToCollateralStep
							}
							payload = map[string]any{"swapInstruction": executionSwap(t, &v, action, amount, amount).Instruction}
						default:
							t.Fatalf("unexpected request: %s", req.URL.Path)
						}
						raw, _ := json.Marshal(payload)
						return response(string(raw)), nil
					}))
					if err != nil {
						t.Fatal(err)
					}
					st := stepState{route: route, collateral: reserve, topology: []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}, conversionCap: uint64(executionTradeValueRaw), maxSlippageBPS: 20, collateralBalance: held}
					e, _, err := assembleStep(context.Background(), rpc, fixtureView(t, rpc), client, v.manifest, policies, st, kind, 5_000_000_000, 5_000_000_000+held, o.Snapshot.Slot)
					if err != nil {
						t.Fatal(err)
					}
					if quotes != 1 || e.Request.Swap == nil || e.Request.Swap.AmountRaw != uint64(executionTradeValueRaw) || e.Request.Open.AmountRaw > uint64(executionTradeValueRaw) {
						t.Fatal("portion was not capped before quoting", e.Request)
					}
					if (kind == stepExit) != (e.Request.Close == nil) {
						t.Fatal("atomic closing leg lost")
					}
					d.Action, d.Reason = DeleverRouteStep, debtZeroReason
					if kind == stepUp {
						d.Action, d.Reason = OpenRouteStep, leverageUpReason
						o.Snapshot.Target.Rule = ruleEntry
						bindSnapshotNAV(&o.Snapshot)
					}
					guarded, err := protectExecutionPrice(&v, o, d, e.Request)
					if err != nil {
						t.Fatal(err)
					}
					request := guarded.(StepRequest)
					if _, _, _, err := compileRequest(v.manifest, request); err != nil {
						t.Fatal(err)
					}
					message, err := compileStepMessageForDelegate(&v, request, delegate)
					if err != nil {
						t.Fatal(err)
					}
					signed, err := signMessage(key, delegate, message, request.Open.RecentBlockhash, request.Open.LastValidBlockHeight)
					if err != nil {
						t.Fatal(err)
					}
					built, err := signed.BuildResult(o.Snapshot.Slot)
					if err != nil {
						t.Fatal(err)
					}
					if err := built.validateForDelegate(delegate); err != nil {
						t.Fatal("atomic persist/sign gate", err)
					}
				})
			}
		}
	}
}

func executionSwap(t *testing.T, v *Vault, action Action, in, out uint64) JupiterSwapRequest {
	t.Helper()
	r := stepTestSwap(t, MapleSyrupUSDCLaneID, action, 0)
	r.AmountRaw, r.QuotedOutputRaw, r.MinimumOutputRaw, r.MaxSlippageBPS = in, out, jupiterOutputFloor(out, 20), 20
	data, err := base64.StdEncoding.DecodeString(r.Instruction.Data)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[jupiter.V2InAmountOffset:], in)
	binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], out)
	binary.LittleEndian.PutUint16(data[jupiter.V2SlippageOffset:], 20)
	r.Instruction.Data = base64.StdEncoding.EncodeToString(data)
	_, _, from, to, err := jupiterEdgeForRoute(v, action, r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	r.Instruction.Accounts[1].Pubkey, r.Instruction.Accounts[2].Pubkey, r.Instruction.Accounts[5].Pubkey = v.squads, from, to
	return r
}

func TestExecutionPriceRejectsInitialDiscountAndSealsBothVaultWires(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, original := range []*Vault{rwa, looping} {
		v := *original
		v.ProtectExecution = true
		v.manifest.vault = &v
		for _, action := range []Action{SwapCollateralToStableStep, SwapStableToCollateralStep} {
			for _, quote := range []uint64{990_000_000, 998_000_000, 999_000_000, 1_000_000_000} {
				t.Run(fmt.Sprintf("%s/%s/%d", v.Name, action, quote), func(t *testing.T) {
					o, d := executionFixture(t, &v)
					d.Action, d.AmountRaw = action, executionTradeValueRaw
					if action == SwapStableToCollateralStep {
						o.Snapshot.Target = LaneTarget{Rule: ruleEntry}
						bindSnapshotNAV(&o.Snapshot)
					}
					r := executionSwap(t, &v, action, uint64(d.AmountRaw), quote)
					got, err := protectExecutionPrice(&v, o, d, r)
					if quote < 998_000_000 {
						var hold *PreSendHold
						if !errors.As(err, &hold) || holdCause(hold) != "withdrawal_waiting_price" {
							t.Fatalf("bad initial quote accepted: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					guarded := got.(JupiterSwapRequest)
					if guarded.MinimumOutputRaw < 998_000_000 {
						t.Fatal("quote loss and execution tolerance accumulated")
					}
					if _, _, _, err := compileRequest(v.manifest, guarded); err != nil {
						t.Fatal("sealed production request", err)
					}
					message, err := compileJupiterMessageForDelegate(&v, guarded, delegate)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, ix := range decodeTestInstructions(t, message) {
						if ix.ProgramID != squads.ProgramID {
							continue
						}
						execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
						if err != nil {
							t.Fatal(err)
						}
						for _, inner := range execution.Inner {
							if inner.ProgramID != jupiter.ProgramID {
								continue
							}
							args, err := jupiter.DecodeSharedAccountsRouteV2Args(inner.Data)
							if err != nil || jupiterOutputFloor(args.QuotedOutAmount, args.SlippageBPS) < 998_000_000 {
								t.Fatal("wire lost reference-price floor", err)
							}
							found = true
						}
					}
					if !found {
						t.Fatal("no swap in signed message")
					}
					signed, err := signMessage(key, delegate, message, guarded.RecentBlockhash, guarded.LastValidBlockHeight)
					if err != nil {
						t.Fatal(err)
					}
					build, err := signed.BuildResult(o.Snapshot.Slot)
					if err != nil {
						t.Fatal(err)
					}
					if err = build.validateForDelegate(delegate); err != nil {
						t.Fatal("persist/sign validation", err)
					}
					changed := guarded
					changed.Instruction = withTestSlippage(changed.Instruction, 50)
					if _, _, _, err := compileRequest(v.manifest, changed); err == nil {
						t.Fatal("changed wire escaped price seal")
					}
					guarded.admission.net.validUntil = time.Now().Add(-time.Second)
					if _, _, _, err := compileRequest(v.manifest, guarded); err == nil {
						t.Fatal("expired valuation signed")
					}
				})
			}
		}
	}
}

func TestExecutionSizingKeepsDemandAndReturnsCashBeforeNextSale(t *testing.T) {
	v := *rwa
	v.ProtectExecution = true
	o, d := executionFixture(t, &v)
	bounded, err := boundedExecutionDecision(&v, o, d)
	if err != nil || bounded.AmountRaw != executionTradeValueRaw || o.Snapshot.WithdrawalDemandRaw != 5_000_000_000 {
		t.Fatalf("sale sizing changed intent: %+v %v", bounded, err)
	}
	o.Snapshot.SquadsIdleRaw = 400_000_000
	bindSnapshotNAV(&o.Snapshot)
	bounded, err = boundedExecutionDecision(&v, o, d)
	if err != nil || bounded.Action != StageSquadsToVoltr || bounded.AmountRaw != 400_000_000 {
		t.Fatalf("cash waited behind another sale: %+v %v", bounded, err)
	}
	o.Snapshot.SquadsIdleRaw = 0
	o.Snapshot.Target = LaneTarget{Rule: ruleEntry}
	d.Action = SwapStableToCollateralStep
	bounded, err = boundedExecutionDecision(&v, o, d)
	if err != nil || bounded.AmountRaw != executionTradeValueRaw {
		t.Fatal("large deposit not split", bounded, err)
	}
	d.Action = VoltrAllocateToSquads
	if _, err := boundedExecutionDecision(&v, o, d); err == nil {
		t.Fatal("stored large entry quote was allocated without repricing")
	}
	o.Snapshot.Target = LaneTarget{Rule: ruleWithdrawal}
	o.Snapshot.PositionCollateralValueRaw, o.Snapshot.PositionDebtValueRaw = 30_000_000_000, 20_000_000_000
	d.Action, d.Reason = DeleverRouteStep, releaseReason
	bounded, err = boundedExecutionDecision(&v, o, d)
	if err != nil || bounded.AmountRaw != 333_333_333 {
		t.Fatal("borrowed exposure ignored by release sizing", bounded, err)
	}
	for _, rule := range []targetRule{ruleHardLTV, ruleTermMaturity, ruleOperatorExit} {
		o.Snapshot.Target.Rule = rule
		bounded, err = boundedExecutionDecision(&v, o, d)
		if err != nil || bounded != d {
			t.Fatal("urgent/operator action changed", rule, err)
		}
		if _, err := protectExecutionPrice(&v, Observation{Snapshot: o.Snapshot}, d, StepRequest{}); err != nil {
			t.Fatal("emergency demanded a quote", err)
		}
	}
}

func TestExecutionLimitUsesTokenDecimalsAndNonUSDCPrices(t *testing.T) {
	two, half := kaminoOne(), kaminoOne()
	binary.LittleEndian.PutUint64(two[:8], 2<<60)
	binary.LittleEndian.PutUint64(half[:8], 1<<59)
	prices := lanePrices{collateral: two, debt: half, usdc: kaminoOne(), collateralDecimals: 9, debtDecimals: 6}
	for mint, want := range map[string]uint64{fleet.USDCMint: 1_000_000_000, autoAUTOPYUSD.Kamino.CollateralMint: 500_000_000_000, autoAUTOPYUSD.Kamino.DebtMint: 2_000_000_000} {
		got, err := executionTokenLimit(prices, autoAUTOPYUSD, mint)
		if err != nil || got != want {
			t.Fatalf("%s: limit %d want %d: %v", mint, got, want, err)
		}
	}
	if _, err := executionTokenLimit(prices, autoAUTOPYUSD, "unknown"); err == nil {
		t.Fatal("unknown asset priced as USDC")
	}
}

func TestExecutionPacingUsesDurableVaultScopedJournal(t *testing.T) {
	db := withdrawalCostTestDB(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := db.executionReady(ctx, rwa.RouteKey, now); err != nil {
		t.Fatal(err)
	}
	_, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,status,signed_wire_sha256,transaction_signature,recent_blockhash,last_valid_block_height,broadcast_intent_at,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects,expected_effects,updated_at)
		VALUES ('trade',$1,'reconciled','hash','signature','blockhash',100,$2,42,'finalized','reconciled','{}','{"expectedEffects":{"kind":"atomic-step"}}',$2)`, rwa.RouteKey, now)
	if err != nil {
		t.Fatal(err)
	}
	// A new Database value models a new worker with no process-local timer.
	restarted := &Database{pool: db.pool}
	if err := restarted.executionReady(ctx, rwa.RouteKey, now.Add(59*time.Second)); err == nil {
		t.Fatal("restart bypassed pacing")
	}
	if err := restarted.executionReady(ctx, looping.RouteKey, now); err != nil {
		t.Fatal("one vault delayed the other", err)
	}
	if err := restarted.executionReady(ctx, rwa.RouteKey, now.Add(time.Minute)); err != nil {
		t.Fatal("completed interval remained held", err)
	}
	_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',recovery_reason='signature_absent_after_blockhash_expiry',simulation_slot=42,simulation_result='{}',confirmed_slot=NULL,updated_at=$1 WHERE operation_id='trade'`, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.executionReady(ctx, rwa.RouteKey, now.Add(61*time.Second)); err == nil {
		t.Fatal("failed broadcast allowed immediate paid retry")
	}
	_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET broadcast_intent_at=NULL WHERE operation_id='trade'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.executionReady(ctx, rwa.RouteKey, now.Add(61*time.Second)); err != nil {
		t.Fatal("unbroadcast refusal treated as a fill", err)
	}
}

func TestPublicWithdrawalRetainsAutomaticFloorWithoutConsent(t *testing.T) {
	old := rwa.ProtectExecution
	rwa.ProtectExecution = true
	t.Cleanup(func() { rwa.ProtectExecution = old })
	o, d := executionFixture(t, rwa)
	d.AmountRaw = executionTradeValueRaw
	o.Snapshot.net.quotes = []ExitCost{{Lane: d.StrategyKey, MarkSF: kaminoOne(), QuotedCollateralRaw: 10_000_000_000, BPS: microBPSWhole / 1000,
		Source: exitCostQuote, Slot: o.Snapshot.Slot, At: time.Now(), scope: &exitQuoteScope{vault: rwa.squads, manifest: rwa.manifest.SHA256, path: quotePath(rwa.manifest, d.StrategyKey), usdc: kaminoOne()}}}
	r := executionSwap(t, rwa, d.Action, uint64(d.AmountRaw), 998_500_000)
	effects := ExpectedEffects{Sale: &ExpectedSale{Lane: d.StrategyKey, SoldRaw: r.AmountRaw, MarkSF: kaminoOne(), MarkValueRaw: uint64(d.AmountRaw)}}
	if _, err := prepareWithdrawalCost(context.Background(), nil, nil, nil, rwa.manifest, d, o, r, effects); err == nil {
		t.Fatal("sale below the NAV allowance was accepted")
	}
	r = executionSwap(t, rwa, d.Action, uint64(d.AmountRaw), 999_500_000)
	got, err := prepareWithdrawalCost(context.Background(), nil, nil, nil, rwa.manifest, d, o, r, effects)
	if err != nil {
		t.Fatal("public sale required consent/database", err)
	}
	if _, wrapped := got.(withdrawalCostRequest); wrapped {
		t.Fatal("public consent wrapper restored")
	}
	if floor, _ := jupiterInstructionWireFloor(got.(JupiterSwapRequest).Instruction); floor < 999_000_000 {
		t.Fatal("NAV sale floor missing", floor)
	}
	if _, _, _, err := compileRequest(rwa.manifest, got); err != nil {
		t.Fatal("restored floor invalidated final wire", err)
	}
	if request := releaseRequestOf(rwa, o.Snapshot, d, got); request == nil || string(request.action) != "ordinary_funding" {
		t.Fatal("operator funding gate bypassed")
	}
}

func TestExecutionRecoveryPrecedesNewTradePacing(t *testing.T) {
	v := *rwa
	v.ProtectExecution = true
	v.manifest.vault = &v
	recovered := false
	w := &Worker{manifest: v.manifest, routeKey: v.RouteKey, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			return &PersistedOperation{Status: Signed, Operation: Operation{Decision: Decision{Action: DeleverRouteStep}}}, nil
		},
		advance:        func(context.Context, PersistedOperation) error { recovered = true; return nil },
		executionReady: func(context.Context, string, time.Time) error { t.Fatal("pacing touched signed recovery"); return nil },
		observe: func(context.Context) (Observation, error) {
			t.Fatal("signed operation re-planned")
			return Observation{}, nil
		},
	}}
	if err := w.Tick(context.Background()); err != nil || !recovered {
		t.Fatal(err)
	}
}

func TestExecutionTickWaitsThenRequotesAndReturnsCollectedCash(t *testing.T) {
	v := *rwa
	v.ProtectExecution, v.manifest.vault = true, &v
	o, _ := executionFixture(t, &v)
	if d := Decide(o.Snapshot); d.Action != SwapCollateralToStableStep {
		t.Fatal("fixture decision", d)
	}
	waiting, quote := true, uint64(990_000_000)
	prepared, recorded, built := 0, 0, 0
	w := &Worker{routeKey: v.RouteKey, manifest: v.manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return o, nil },
		executionReady: func(context.Context, string, time.Time) error {
			if waiting {
				return staleHold("execution_waiting_interval")
			}
			return nil
		},
		prepareJupiter: func(_ context.Context, _ RouteManifest, d Decision, ob Observation) (Observation, JupiterExecutionEvidence, error) {
			prepared++
			if d.AmountRaw != executionTradeValueRaw {
				t.Fatal("uncapped trade reached quoting", d)
			}
			return ob, JupiterExecutionEvidence{Request: executionSwap(t, &v, d.Action, uint64(d.AmountRaw), quote)}, nil
		},
		prepareBridge: func(_ context.Context, _ RouteManifest, d Decision, ob Observation) (Observation, BridgeExecutionEvidence, error) {
			if d.Action != StageSquadsToVoltr || d.AmountRaw != 400_000_000 {
				t.Fatal("collected cash not returned", d)
			}
			return ob, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: bridgeStageSquadsToVoltr}}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, *releaseRequest) (DecisionRecord, error) {
			recorded++
			return DecisionRecord{OperationID: "portion", Status: Decided}, nil
		},
		build: func(_ context.Context, _ string, r any, _ ExpectedEffects) error {
			built++
			if swap, ok := r.(JupiterSwapRequest); ok {
				_, _, _, err := compileRequest(v.manifest, swap)
				return err
			}
			return nil
		},
	}}
	if err := w.Tick(context.Background()); err == nil || prepared != 0 || built != 0 {
		t.Fatal("wait allowed new work", err)
	}
	waiting = false
	if err := w.Tick(context.Background()); err == nil || prepared != 1 || recorded != 0 || built != 0 {
		t.Fatal("bad quote reached the journal/signer", err)
	}
	quote = 999_000_000
	if err := w.Tick(context.Background()); err != nil || prepared != 2 || recorded != 1 || built != 1 {
		t.Fatal("fresh acceptable quote did not proceed", err)
	}
	waiting = true
	o.Snapshot.SquadsIdleRaw = 400_000_000
	bindSnapshotNAV(&o.Snapshot)
	if err := w.Tick(context.Background()); err != nil || prepared != 2 || recorded != 2 || built != 2 {
		t.Fatal("pacing delayed available cash", err)
	}
}
