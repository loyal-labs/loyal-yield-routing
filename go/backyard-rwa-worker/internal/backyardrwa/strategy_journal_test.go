package backyardrwa

import (
	"compress/gzip"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestStrategyJournalAssociationAgainstAuditedInventory(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	migration, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/0080_backyard_rwa_strategy_journal.sql")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("../../../../docs/evidence/voltr-selector-2026-09-16/strategy-one-journal-inventory.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	inventory, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, productionRouteKey)
	exec(`INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}') ON CONFLICT(route_key) DO UPDATE SET lease_owner=NULL,lease_expires_at=NULL`, productionRouteKey)
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,engine_version,action,status,confirmed_slot,transaction_signature,expected_effects,signed_wire)
 SELECT operation_id,route_key,engine_version,action,status,confirmed_slot,transaction_signature,
 '{"original":true,"decision":{"observationSlot":444157800,"amountRaw":793417}}',decode('010203','hex')
 FROM jsonb_to_recordset($1::jsonb) AS x(operation_id text,route_key text,engine_version text,action text,status text,confirmed_slot bigint,transaction_signature text)`, string(inventory))
	exec(`CREATE TEMP TABLE strategy_before ON COMMIT DROP AS SELECT * FROM loyal_yield.multiply_operations WHERE route_key=$1`, productionRouteKey)
	// Each rejected migration runs in a savepoint; no labels may survive it.
	reject := func(name, mutation string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			nested, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer nested.Rollback(ctx)
			if _, err = nested.Exec(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if _, err = nested.Exec(ctx, string(migration)); err == nil {
				t.Fatal("accepted unaudited/concurrent history")
			}
		})
	}
	reject("changed_signature", `UPDATE loyal_yield.multiply_operations SET transaction_signature='different' WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`)
	reject("changed_action", `UPDATE loyal_yield.multiply_operations SET action='OPEN_ROUTE_STEP' WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`)
	reject("extra_old_row", `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,engine_version,action,status,confirmed_slot,transaction_signature,expected_effects) SELECT 'unexpected-old',route_key,engine_version,action,status,confirmed_slot,transaction_signature,expected_effects FROM strategy_before LIMIT 1`)
	reject("missing_row", `DELETE FROM loyal_yield.multiply_operations WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`)
	reject("later_unscoped", `UPDATE loyal_yield.multiply_operations SET confirmed_slot=446086070 WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`)
	reject("active_lease", `UPDATE loyal_yield.multiply_route_states SET lease_owner='live',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE route_key='`+productionRouteKey+`'`)
	reject("nonterminal", `UPDATE loyal_yield.multiply_operations SET status='signed' WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`)
	exec(string(migration))
	exec(string(migration))
	var changed int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations a JOIN strategy_before b USING(operation_id)
 WHERE (to_jsonb(a)-'expected_effects') IS DISTINCT FROM (to_jsonb(b)-'expected_effects')
 OR (a.expected_effects-'journalStrategyConfig'-'journalAssociation') IS DISTINCT FROM b.expected_effects`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("history rewritten: %d %v", changed, err)
	}
	count := func(q pgx.Tx, want int) {
		t.Helper()
		var got int
		if err := q.QueryRow(ctx, ActiveStrategyJournalCTE+`SELECT count(*) FROM strategy_journal WHERE route_key=$1`, productionRouteKey).Scan(&got); err != nil || got != want {
			t.Fatalf("visible rows: got%d want%d %v", got, want, err)
		}
	}
	count(tx, 0)
	rows, err := tx.Query(ctx, ReconciledBridgeJournalSQL, productionRouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing journal result")
	}
	values, err := rows.Values()
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, v := range values {
		if v != nil {
			t.Fatalf("retired journal contaminated fresh receipt: %v", values)
		}
	}
	// A copied, incomplete, or mutated association never silently erases evidence.
	cases := map[string]string{
		"missing_scope":         `expected_effects-'journalStrategyConfig'`,
		"missing_association":   `expected_effects-'journalAssociation'`,
		"malformed_association": `jsonb_set(expected_effects,'{journalAssociation}','[]')`,
		"copied_identity":       `jsonb_set(expected_effects,'{journalAssociation,operationId}','"different"')`,
		"wrong_hash":            `jsonb_set(expected_effects,'{journalAssociation,evidenceSha256}','"different"')`,
		"current_scope":         `jsonb_set(expected_effects,'{journalStrategyConfig}','"` + bridgeStrategy + `"')`,
	}
	for name, expression := range cases {
		t.Run(name, func(t *testing.T) {
			nested, e := tx.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer nested.Rollback(ctx)
			if _, e = nested.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=`+expression+` WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`); e != nil {
				t.Fatal(e)
			}
			count(nested, 1)
		})
	}
	for name, mutation := range map[string]string{
		"later_slot":        "confirmed_slot=446086070",
		"changed_signature": "transaction_signature='changed'",
		"changed_status":    "status='failed'",
		"changed_action":    "action='HOLD_MANUAL_RECOVERY'",
	} {
		t.Run("bound_"+name, func(t *testing.T) {
			nested, e := tx.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer nested.Rollback(ctx)
			if _, e = nested.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET `+mutation+` WHERE operation_id=(SELECT min(operation_id) FROM strategy_before)`); e != nil {
				t.Fatal(e)
			}
			count(nested, 1)
		})
	}
	// Current operations drive NAV cadence regardless of the retired report volume.
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,engine_version,action,status,confirmed_slot,expected_effects) VALUES('current-open',$1,'backyard_rwa_v1','OPEN_ROUTE_STEP','reconciled',446086071,'{}')`, productionRouteKey)
	var dirty bool
	if err = tx.QueryRow(ctx, PostMutationNAVRequiredSQL, productionRouteKey).Scan(&dirty); err != nil || !dirty {
		t.Fatal("current mutation not dirty", err)
	}
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,engine_version,action,status,confirmed_slot,expected_effects) VALUES('current-report',$1,'backyard_rwa_v1','REPORT_NAV','reconciled',446086072,'{}')`, productionRouteKey)
	if err = tx.QueryRow(ctx, PostMutationNAVRequiredSQL, productionRouteKey).Scan(&dirty); err != nil || dirty {
		t.Fatal("current report did not clean NAV", err)
	}
	count(tx, 2)
}
