package backyard

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

func TestLoopingFeeJournalFinalityAndRestart(t *testing.T) {
	dsn := os.Getenv("BACKYARD_FOREIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable local backyard_foreign_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(cfg.ConnConfig.Database, "backyard_foreign_test") {
		t.Fatal("requires private Looping test database")
	}
	ctx := t.Context()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"testdata/withdrawal-cost-parent.sql", "../../../../migrations/yield/0150_backyard_looping_performance_fee.sql"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(data))
		if strings.HasSuffix(file, "parent.sql") {
			defer exec("DROP SCHEMA loyal_yield CASCADE")
			exec(`CREATE TABLE loyal_yield.multiply_route_states (route_key text PRIMARY KEY,state jsonb,updated_at timestamptz)`)
		}
	}
	exec(`CREATE TABLE loyal_yield.backyard_lane_levels(route_key text,lane text,level numeric,up_approved boolean,down_approval jsonb);
        CREATE TABLE loyal_yield.backyard_lane_exits(route_key text,lane text,equity_zero boolean);
        CREATE TABLE loyal_yield.backyard_lane_exit_costs(lane text,source text,bps_micro bigint,quoted_collateral_raw bigint,mark_sf numeric, slot bigint,recorded_at timestamptz)`)
	_, x := feeWireFixture(t)
	before := x.PerformanceFee.Before
	jsonOf := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	state := func(f performanceFeeState) {
		exec(`UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{performanceFee}',$2::jsonb) WHERE route_key=$1`, looping.RouteKey, jsonOf(f))
	}
	exec(`INSERT INTO loyal_yield.multiply_route_states VALUES($1,'{"other":9}',now()),($2,'{"other":2}',now())`, looping.RouteKey, rwa.RouteKey)
	state(before)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{performanceFee,weeklyCapRaw}','10000000001'::jsonb) WHERE route_key=$1`, looping.RouteKey); err == nil {
		t.Fatal("database persisted above-ceiling transfer authority")
	}
	// A prior finalized snapshot and a pending cash payment survive a new worker.
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id, route_key,status,action,confirmed_slot,expected_effects) VALUES('prior',$1,'reconciled','REPORT_NAV',500,$2::jsonb)`, looping.RouteKey, jsonOf(map[string]any{"expectedEffects": map[string]any{"performanceFee": map[string]any{"after": before}}}))
	op := func(id string) {
		exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,confirmed_slot,transaction_signature,expected_effects)
            VALUES($1,$2,'confirmed','PAY_PERFORMANCE_FEE',$3,'fee-fixture',$4::jsonb)`, id, looping.RouteKey, x.PerformanceFee.Slot+1, jsonOf(map[string]any{"expectedEffects": x}))
	}
	op("pay")
	db := &Database{pool: pool}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action) VALUES('wrong',$1,'decided','PAY_PERFORMANCE_FEE')`, rwa.RouteKey); err == nil {
		t.Fatal("schema accepted payment for Earn Max")
	}
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,confirmed_slot,reconciled_effects) VALUES('sale',$1,'reconciled','SWAP_COLLATERAL_TO_STABLE_STEP',501,'{"loopingCashDelta":7}')`, looping.RouteKey)
	read := func(want performanceFeeState, delta int64) {
		t.Helper()
		p, err := db.readRoutePlanningState(ctx, looping.RouteKey)
		if err != nil || p.inputs.performanceFee == nil || *p.inputs.performanceFee != want || p.inputs.performanceCashDelta != delta {
			t.Fatalf("restart checkpoint or actual cash movements changed: %+v / %v", p, err)
		}
	}
	read(before, 7)
	r, body, err := ReconcileConfirmedTransaction(looping, x, feeFinalReceipt(x))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkReconciled(ctx, "pay", r, []byte(`{}`)); err == nil {
		t.Fatal("omitted fee committed")
	}
	wrong := r
	wrong.EffectsSHA256 = strings.Repeat("f", 64)
	if err := db.MarkReconciled(ctx, "pay", wrong, body); err == nil {
		t.Fatal("altered receipt committed")
	}
	read(before, 7)
	if err := db.MarkReconciled(ctx, "pay", r, body); err != nil {
		t.Fatal(err)
	}
	after := x.PerformanceFee.After
	read(after, 0)
	if err := db.MarkReconciled(ctx, "pay", r, body); err == nil {
		t.Fatal("settled a finalized transfer twice")
	}
	op("duplicate")
	if err := db.MarkReconciled(ctx, "duplicate", r, body); err == nil {
		t.Fatal("replayed fee version through a new operation")
	}
	exec(`UPDATE loyal_yield.multiply_operations SET status='manual_recovery',signed_wire=decode('00','hex') WHERE operation_id='duplicate'`)
	if _, err := db.readRoutePlanningState(ctx, looping.RouteKey); err == nil {
		t.Fatal("ambiguous signed payment allowed another assessment")
	}
	exec(`UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id='duplicate'`)
	read(after, 0)
	t.Run("external receipt approval and one finalized rebase", func(t *testing.T) {
		var sig solana.Signature
		sig[0] = 74
		report := func(id string, s Snapshot, gross int64) performanceFeeState {
			t.Helper()
			e := feeStep(t, *s.performanceFee, s, gross, gross, s.SquadsIdleRaw, 0, true)
			effects, _, _, _, err := bridgeExpectedEffects(looping, Decision{Action: ReportNAV}, 0, 0, uint64(s.SquadsIdleRaw))
			if err != nil {
				t.Fatal(err)
			}
			effects.Kind = effectsBridge
			effects.PerformanceFee = &e
			effects.ReturnData = expectedAdaptorReturnData(looping, uint64(e.After.PublishedStrategy))
			receipt := feeFinalReceipt(effects)
			receipt.Signature = id
			exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,confirmed_slot,transaction_signature,expected_effects)
                  VALUES($1,$2,'confirmed','REPORT_NAV',$3,$1,$4::jsonb)`, id, looping.RouteKey, receipt.Slot, jsonOf(map[string]any{"expectedEffects": effects}))
			proof, body, err := ReconcileConfirmedTransaction(looping, effects, receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.MarkReconciled(ctx, id, proof, body); err != nil {
				t.Fatal(err)
			}
			return e.After
		}
		s := feeBank(after, after.LastAssessment+3600, after.PublishedSupply, after.PublishedValue, after.PublishedCash)
		s.Slot = 601
		s.feeCapitalSlot = 601
		s.performanceFee = &after
		s.feeCapitalHold = sig.String()
		held := report("held", s, after.PublishedValue+100)
		basis := feeCapitalResolution{sig.String(), 600, 100, held.PublishedSupply}
		expect := func(apply bool, want string) {
			t.Helper()
			got, err := db.recordFeeResolution(ctx, basis, "reviewer", "finalized receipt and LP valuation", apply)
			if err != nil || got != want {
				t.Fatal(got, err)
			}
		}
		wrong := basis
		wrong.Signature = foreignInflowAnchor
		if _, err := db.recordFeeResolution(ctx, wrong, "reviewer", "other receipt", true); err == nil {
			t.Fatal("unrelated approval accepted")
		}
		expect(false, "dry-run")
		expect(true, "recorded")
		expect(true, "already-recorded")
		changed := basis
		changed.Principal++
		if _, err := db.recordFeeResolution(ctx, changed, "reviewer", "finalized receipt and LP valuation", true); err == nil {
			t.Fatal("basis silently replaced")
		}
		s = feeBank(held, held.LastAssessment+3600, held.PublishedSupply, held.PublishedValue, held.PublishedCash)
		s.Slot = 610
		s.performanceFee = &held
		silent := &View{accounts: map[string]viewAccount{}}
		for address := range feeCapitalAccounts() {
			silent.accounts[address.String()] = viewAccount{slot: uint64(held.CapitalSlot) - 1}
		}
		db.observeFeeCapital(ctx, nil, silent, &s)
		if s.feeResolution != basis || s.feeCapitalHold != "" {
			t.Fatal("worker failed to read the approved basis", s.feeResolution)
		}
		done := report("rebase", s, held.PublishedValue+10)
		if done.Outstanding != held.Outstanding || done.CapitalHold != "" || done.CapitalSlot != 610 {
			t.Fatal(done)
		}
		expect(true, "already-applied")
		read(done, 0)
	})

	state(before)
	if _, err := db.readRoutePlanningState(ctx, looping.RouteKey); err == nil {
		t.Fatal("accepted rolled-back state")
	}
	exec(`UPDATE loyal_yield.multiply_route_states SET state=state-'performanceFee' WHERE route_key=$1`, looping.RouteKey)
	if _, err := db.readRoutePlanningState(ctx, looping.RouteKey); err == nil {
		t.Fatal("missing checkpoint silently resumed gross pricing")
	}
	var intact bool
	if err := pool.QueryRow(ctx, `SELECT state='{"other":2}'::jsonb FROM loyal_yield.multiply_route_states WHERE route_key=$1`, rwa.RouteKey).Scan(&intact); err != nil || !intact {
		t.Fatal("Earn Max changed", err)
	}
}
