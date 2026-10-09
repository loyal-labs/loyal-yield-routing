package backyard

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
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func partialRepaymentFixture(t *testing.T, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	return partialRepaymentFixtureForLane(t, SelectedRouteID, variant)
}

// partialRepaymentFixtureForLane: on a B2 leverage lane the same position is
// an exit cycle (unwinding at the release ceiling, below hard LTV), so the
// decision is exit_partial_repay instead of hard_ltv_partial_repay.
func partialRepaymentFixtureForLane(t *testing.T, lane, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixtureForLane(t, lane)
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 80
	accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset+1] = 90
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 45_000_000)
	o.Snapshot.PilotActive = true
	cash, amount := uint64(500), uint64(500)
	if variant == "funded" {
		cash, amount = 1001, 999
	}
	o.Snapshot.SquadsIdleRaw = int64(cash)
	o.Snapshot.LTVBPS = 6000
	o.Snapshot.LiquidationThresholdBPS = 8000
	o.Snapshot.PayoffDebtRaw = 1006
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], cash)
	want := "hard_ltv_partial_repay"
	if leverageLane(lane) {
		o.Snapshot.LTVBPS, o.Snapshot.Unwind = 5500, true
		o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.CollateralIdleValueRaw = 0, 0, 0
		want = exitPartialRepayReason
	}
	d := Decide(o.Snapshot)
	if d.Reason != want || d.AmountRaw != int64(amount) {
		t.Fatalf("partial decision: %+v snapshot %+v", d, o.Snapshot)
	}
	r, err := m.kaminoPacketForRoute(d.Action, kaminoLegRepay, uint64(d.AmountRaw), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	src, dst := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, src, dst, amount, amount)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	addresses := append(depositProjectionAddresses(route), route.Kamino.DebtReserve, route.DebtLiquiditySupply)
	_, full, err := confirmedAccounts(context.Background(), rpc, addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	underlying := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(body, &call) != nil {
			t.Fatal("RPC")
		}
		if call.Method != "simulateTransaction" {
			return underlying.RoundTrip(req)
		}
		var encoded string
		if json.Unmarshal(call.Params[0], &encoded) != nil {
			t.Fatal("wire")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(wire) <= 65 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) {
			t.Fatal("changed partial simulation")
		}
		var rows []any
		for _, original := range full {
			a := original
			a.Data = append([]byte(nil), a.Data...)
			for _, effect := range effects.Accounts {
				if a.Address == effect.Address {
					binary.LittleEndian.PutUint64(a.Data[64:72], effect.AfterRaw)
				}
			}
			if a.Address == route.Kamino.Obligation {
				debt := uint64(1000) - amount
				if variant == "debt" {
					debt = 1000
				}
				putScaledFraction(a.Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
				if variant == "receipts" {
					binary.LittleEndian.PutUint64(a.Data[128:136], 1)
				}
			}
			if a.Address == route.CollateralCustody && variant == "collateral" {
				binary.LittleEndian.PutUint64(a.Data[64:72], 999)
			}
			if a.Address == route.DebtCustody && variant == "cash" {
				binary.LittleEndian.PutUint64(a.Data[64:72], 1)
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		var failure any
		if variant == "failed" {
			failure = "simulation failed"
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"err": failure, "unitsConsumed": 211899, "accounts": rows}}})
		return response(string(data)), nil
	})
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client
}

func TestPartialRepaymentAdmissionPricesRemainingExit(t *testing.T) {
	o, d, e, m, rpc, client := partialRepaymentFixture(t, "")
	p, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.RepaymentProjection == nil || p.Payoff == nil || p.Payoff.ObservedDebtRaw != 500 || p.PayoffRepayment == nil || p.PayoffWithdrawal == nil || p.ExitAfterMicros <= 0 || len(p.Exit) == 0 || p.Snapshot != o.Snapshot {
		t.Fatalf("incomplete partial exit: %+v", p)
	}
	if _, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &p, 42); err != nil {
		t.Fatal("fresh partial revalidation", err)
	}
}
func TestPartialRepaymentRejectsProjectedDrift(t *testing.T) {
	for _, variant := range []string{"debt", "receipts", "collateral", "cash", "failed"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, client := partialRepaymentFixture(t, variant)
			if _, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("changed projection admitted")
			}
		})
	}
}
func TestPartialRepaymentDecisionPrincipalCashAndDust(t *testing.T) {
	o, _, _, _, _, _ := partialRepaymentFixture(t, "")
	s := o.Snapshot
	s.SquadsIdleRaw = s.PositionDebtRaw
	s.PayoffDebtRaw = s.PositionDebtRaw + 1
	s.CollateralIdleRaw = 1
	s.PrimeIdleRaw = 1
	d := Decide(s)
	if d.Reason != "hard_ltv_partial_repay" || d.AmountRaw != s.PositionDebtRaw-1 {
		t.Fatal(d)
	}
	s.SquadsIdleRaw = s.PayoffDebtRaw
	d = Decide(s)
	if d.Reason != "hard_ltv_repay" || d.AmountRaw != s.PositionDebtRaw {
		t.Fatal(d)
	}
	s.PilotActive = false
	s.SquadsIdleRaw = 500
	d = Decide(s)
	if d.Reason == "hard_ltv_partial_repay" {
		t.Fatal("pilot authority bypass")
	}
}

func TestPartialRepaymentFundedTailRevalidatesPriceAndBacking(t *testing.T) {
	for _, drift := range []string{"", "price", "backing", "debt"} {
		t.Run(drift, func(t *testing.T) {
			o, d, e, m, rpc, client := partialRepaymentFixture(t, "funded")
			p, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
			if err != nil {
				t.Fatal(err)
			}
			if p.BorrowRelease != nil || p.FundingRelease != nil {
				t.Fatal("fixture must have funded remaining payoff")
			}
			original := rpcOf(rpc).Transport
			rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				res, err := original.RoundTrip(req)
				if err != nil {
					return res, err
				}
				body, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				var payload map[string]any
				if json.Unmarshal(body, &payload) != nil {
					t.Fatal("response")
				}
				result, _ := payload["result"].(map[string]any)
				value, _ := result["value"].(map[string]any)
				rows, _ := value["accounts"].([]any)
				for i, row := range rows {
					address := p.RepaymentProjection.Accounts[i].Address
					a := row.(map[string]any)
					data := a["data"].([]any)
					raw, _ := base64.StdEncoding.DecodeString(data[0].(string))
					route, _ := runtimeRoute(o.Snapshot.RouteLane)
					if address == route.Kamino.CollateralReserve && drift == "price" {
						raw[248] ^= 1
					}
					if address == route.Kamino.CollateralReserve && drift == "backing" {
						binary.LittleEndian.PutUint64(raw[224:232], binary.LittleEndian.Uint64(raw[224:232])+1000)
					}
					if address == route.Kamino.Obligation && drift == "debt" {
						putScaledFraction(raw[1296:1312], new(big.Int).Lsh(big.NewInt(1000), 60))
					}
					data[0] = base64.StdEncoding.EncodeToString(raw)
				}
				body, _ = json.Marshal(payload)
				res.Body = io.NopCloser(bytes.NewReader(body))
				return res, nil
			})
			_, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &p, 42)
			if (drift == "") != (err == nil) {
				t.Fatalf("drift %q: %v", drift, err)
			}
		})
	}
}

func TestPartialRepaymentUnverifiedRiskCannotCommitUnwind(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	o, decision, e, m, rpc, client := partialRepaymentFixture(t, "funded")
	key := fmt.Sprintf("partial-unwind-%d", time.Now().UnixNano())
	id := key + "-operation"
	prior := emptyTestBudget()
	prior.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000}
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	authority := pilotTestAuthority(prior)
	authority.Generation, authority.FinalizedSlot, authority.FlatEvidenceSHA256 = 2, flat.Slot, sha256Bytes(flatJSON)
	budget, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	budget.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000, ExitMicros: 90_000_000}
	raw, _ := json.Marshal(map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{authority, previous, flat}})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, decision, m.SHA256, *m.PolicyCatalog.SHA256)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, decision.Action, decision.StrategyKey, string(envelope)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "partial", time.Minute); err != nil {
		t.Fatal(err)
	}
	// This historical fixture hand-sets LTV and has no coherent observer
	// batch. A projected partial repay cannot manufacture emergency authority.
	assertBudgetHold(t, db.admitPhase3Withdrawal(ctx, rpc, client, m, id, o, decision, e), "debt_clear_emergency_evidence_unavailable")
	var unwind, admitted, wire, sent bool
	if err = db.pool.QueryRow(ctx, `SELECT route.state->'selectorUnwind' IS NOT NULL,op.expected_effects ? 'phase3',op.signed_wire IS NOT NULL,op.broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE operation_id=$1`, id).Scan(&unwind, &admitted, &wire, &sent); err != nil {
		t.Fatal(err)
	}
	if unwind || admitted || wire || sent {
		t.Fatal("unverified risk mutated durable capital authority")
	}
}

// B2 1.75x exit cycle: exit_partial_repay on OnRe runs the same measured
// partial-repay admission (projection + complete remaining exit), repays
// less than the whole debt, and writes no unwind intent of its own.
func TestExitPartialRepayAdmissionOnLeverageLane(t *testing.T) {
	o, d, e, m, rpc, client := partialRepaymentFixtureForLane(t, onreONycUSDC, "")
	if d.Reason != exitPartialRepayReason || d.AmountRaw >= o.Snapshot.PositionDebtRaw {
		t.Fatalf("decision %+v", d)
	}
	p, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.RepaymentProjection == nil || p.Payoff == nil || p.PayoffRepayment == nil || p.PayoffWithdrawal == nil || p.ExitAfterMicros <= 0 || len(p.Exit) == 0 {
		t.Fatalf("incomplete exit: %+v", p)
	}
	for _, variant := range []string{"debt", "receipts", "collateral", "cash", "failed"} {
		o, d, e, m, rpc, client := partialRepaymentFixtureForLane(t, onreONycUSDC, variant)
		if _, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
			t.Fatalf("%s: changed projection admitted", variant)
		}
	}
	// A full repayment is never an exit cycle; Maple keeps only hard LTV.
	full := d
	full.AmountRaw = o.Snapshot.PositionDebtRaw
	if _, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, full, e); err == nil {
		t.Fatal("whole-debt repay admitted as a cycle")
	}
	if partialRepaymentLane(SelectedRouteID, exitPartialRepayReason) || !partialRepaymentLane(autoAUTOPYUSD.Lane, exitPartialRepayReason) || partialRepaymentLane(autoAUTOPYUSD.Lane, "hard_ltv_partial_repay") {
		t.Fatal("partial-repay lane scope")
	}
}
