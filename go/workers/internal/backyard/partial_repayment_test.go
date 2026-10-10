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
	r, err := m.kaminoPacketForRoute(testPolicies(t), d.Action, kaminoLegRepay, uint64(d.AmountRaw), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
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

// The bound partial repay leaves debt on the obligation: its simulated
// poststate repays exactly the step and keeps the collateral in place.
func TestPartialRepaymentProjectionLeavesDebt(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, _ := partialRepaymentFixture(t, "")
	projection, err := observePartialRepaymentProjection(context.Background(), rpc, fixtureView(t, rpc), m, o.Snapshot, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	obligation, err := decodeKaminoObligation(accountAt(projection.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.debtRaw != 500 || obligation.collateralDepositedRaw != uint64(o.Snapshot.PositionCollateralRaw) {
		t.Fatalf("partial repay poststate: %+v %v", obligation, err)
	}
}

func TestPartialRepaymentRejectsProjectedDrift(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"debt", "receipts", "collateral", "cash", "failed"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, _ := partialRepaymentFixture(t, variant)
			if _, err := observePartialRepaymentProjection(context.Background(), rpc, fixtureView(t, rpc), m, o.Snapshot, d, e.Request, e.ExpectedEffects); err == nil {
				t.Fatal("changed projection bound")
			}
		})
	}
}

func TestPartialRepaymentDecisionPrincipalCashAndDust(t *testing.T) {
	t.Parallel()
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
}

func TestPartialRepaymentUnverifiedRiskCannotCommitUnwind(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	o, decision, e, m, rpc, _ := partialRepaymentFixture(t, "funded")
	key := fmt.Sprintf("partial-unwind-%d", time.Now().UnixNano())
	id := key + "-operation"
	raw, _ := json.Marshal(map[string]any{"generation": 2})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, decision, m.SHA256)})
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
	assertBudgetHold(t, db.bindOperation(ctx, rpc, fixtureView(t, rpc), m, id, o, decision, e.Request, e.ExpectedEffects), "debt_clear_emergency_evidence_unavailable")
	var unwind, bound, wire, sent bool
	if err = db.pool.QueryRow(ctx, `SELECT route.state->'selectorUnwind' IS NOT NULL,op.expected_effects ? 'phase3',op.signed_wire IS NOT NULL,op.broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE operation_id=$1`, id).Scan(&unwind, &bound, &wire, &sent); err != nil {
		t.Fatal(err)
	}
	if unwind || bound || wire || sent {
		t.Fatal("unverified risk mutated durable capital authority")
	}
}

// B2 1.75x exit cycle: exit_partial_repay on OnRe takes the same measured
// partial-repay proof, repays less than the whole debt, and writes no unwind
// intent of its own.
func TestExitPartialRepayProjectionOnLeverageLane(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, _ := partialRepaymentFixtureForLane(t, onreONycUSDC, "")
	if d.Reason != exitPartialRepayReason || d.AmountRaw >= o.Snapshot.PositionDebtRaw {
		t.Fatalf("decision %+v", d)
	}
	if _, err := observePartialRepaymentProjection(context.Background(), rpc, fixtureView(t, rpc), m, o.Snapshot, d, e.Request, e.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"debt", "receipts", "collateral", "cash", "failed"} {
		o, d, e, m, rpc, _ := partialRepaymentFixtureForLane(t, onreONycUSDC, variant)
		if _, err := observePartialRepaymentProjection(context.Background(), rpc, fixtureView(t, rpc), m, o.Snapshot, d, e.Request, e.ExpectedEffects); err == nil {
			t.Fatalf("%s: changed projection bound", variant)
		}
	}
	// A full repayment is never an exit cycle; Maple keeps only hard LTV.
	full := d
	full.AmountRaw = o.Snapshot.PositionDebtRaw
	if _, err := observePartialRepaymentProjection(context.Background(), rpc, fixtureView(t, rpc), m, o.Snapshot, full, e.Request, e.ExpectedEffects); err == nil {
		t.Fatal("whole-debt repay bound as a cycle")
	}
	if partialRepaymentLane(SelectedRouteID, exitPartialRepayReason) || !partialRepaymentLane(autoAUTOPYUSD.Lane, exitPartialRepayReason) || partialRepaymentLane(autoAUTOPYUSD.Lane, "hard_ltv_partial_repay") {
		t.Fatal("partial-repay lane scope")
	}
}
