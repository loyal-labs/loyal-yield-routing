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
	"testing"
	"time"
)

func partialRepaymentFixture(t *testing.T, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixture(t)
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
	d := Decide(o.Snapshot)
	if d.Reason != "hard_ltv_partial_repay" || d.AmountRaw != int64(amount) {
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
	_, full, err := rpc.GetMultipleAccounts(context.Background(), addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	underlying := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			original := rpc.client.Transport
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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

func TestPartialRepaymentUnwindPersistsWithBudgetAndRollback(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	o, decision, e, m, rpc, client := partialRepaymentFixture(t, "funded")
	plan, err := observePhase3PartialRepaymentAdmission(ctx, rpc, client, m, o, decision, e)
	if err != nil {
		t.Fatal(err)
	}
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
	intent, err := Phase3IntentDigest(e.Request, plan.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: plan.Input, BridgeAdmission: &plan, PilotAuthorityID: budget.Pilot.AuthorityID}
	for _, commit := range []bool{false, true} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := db.readPhase3BudgetTx(ctx, tx, id)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Admit(BudgetReservation{OperationID: id, Family: "Maple", IntentSHA256: intent, UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: true, ExecutionCostUpperMicros: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err = db.persistPartialRepaymentUnwindTx(ctx, tx, plan, b, intent); err != nil {
			t.Fatal(err)
		}
		if err = db.writePhase3BudgetTx(ctx, tx, id, b, auth); err != nil {
			t.Fatal(err)
		}
		err = tx.Rollback(ctx)
		if err == nil && commit {
			err = db.admitPhase3Withdrawal(ctx, rpc, client, m, id, o, decision, e)
		}
		if err != nil {
			t.Fatal(err)
		}
		unwind, err := db.LoadUnwindIntent(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if !commit {
			if unwind != nil {
				t.Fatal("rolled-back unwind escaped")
			}
			continue
		}
		if unwind == nil || unwind.Reason != "hard_ltv_reduction" {
			t.Fatal("missing durable risk continuation")
		}
		// A fresh snapshot after restart/NAV must drain the residual cash, never
		// classify it as another borrowing tranche.
		s := o.Snapshot
		s.PositionDebtRaw = 1
		s.PositionDebtValueRaw = 1
		s.PayoffDebtRaw = 2
		s.SquadsIdleRaw = 2
		s.LTVBPS = 1
		s.PostMutationNAVRequired = false
		if err = applyUnwindIntent(&s, unwind); err != nil {
			t.Fatal(err)
		}
		if got := Decide(s); got.Reason != "withdrawal_repay_debt" || got.Action != DeleverRouteStep {
			t.Fatalf("partial cash re-entered leverage: %+v", got)
		}
		var state struct {
			Budget Phase3Budget `json:"phase3"`
			Paused bool         `json:"selectorEntryPaused"`
		}
		if err = db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(raw, &state) != nil || !state.Paused || state.Budget.Families["Maple"].SpentMicros != 1_000_000 || !state.Budget.Reservations[id].Recovery || state.Budget.Reservations[id].ExitBeforeMicros != 90_000_000 {
			t.Fatal("budget history/continuation drift")
		}
	}
	// Production admission is idempotent, and pre-signing authorization
	// revalidates the retained partial simulation before signer access.
	if err = db.admitPhase3Withdrawal(ctx, rpc, client, m, id, o, decision, e); err != nil {
		t.Fatal("admission retry", err)
	}
	if err = db.authorizePhase3Build(ctx, rpc, id, e.Request, plan.Input.Effects, plan.CurrentCost); err != nil {
		t.Fatal("partial build authorization", err)
	}
	// The production release helper is invoked only after the caller proves
	// this unsigned decided attempt unspent. Keep the risk exit committed.
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.releasePhase3UnspentTx(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	unwind, err := db.LoadUnwindIntent(ctx, key)
	if err != nil || unwind == nil || unwind.Reason != "hard_ltv_reduction" {
		t.Fatal("unspent attempt discarded risk exit", err)
	}
	var restored Phase3Budget
	if err = db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &restored) != nil || restored.Families["Maple"].SpentMicros != 1_000_000 || restored.Families["Maple"].ExitMicros != 90_000_000 || len(restored.Reservations) != 0 {
		t.Fatal("unspent attempt did not restore original reserve")
	}

}
