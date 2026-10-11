package backyard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A route state written by a one-lane release keeps its stored target and
// release sequence: the lease moves each single-slot entry into its lane's
// map entry, unchanged and at the same state version, and per-lane writes
// then keep every other lane's entry.
func TestLeaseMovesSingleSlotLaneStateIntoPerLaneMaps(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("lane-state-%d", time.Now().UnixNano())
	target := LeverageTarget{Lane: autoAUTOPYUSD.Lane, Level: 1.75, SpreadBPS: 391, DecidedAt: time.Now().UTC().Truncate(time.Microsecond)}
	partial := partialWithdrawalState{Lane: autoAUTOPYUSD.Lane, OperationID: strings.Repeat("a", 64), Generation: 1, LTVBPS: 2_000}
	rawTarget, _ := json.Marshal(target)
	rawPartial, _ := json.Marshal(partial)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,jsonb_build_object('generation',1,'cycle',1,'leverageTarget',$2::jsonb,'partialWithdrawal',$3::jsonb))`, key, string(rawTarget), string(rawPartial)); err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key)
	if _, err := db.AcquireRouteLease(ctx, key, "lane-state", time.Minute); err != nil {
		t.Fatal(err)
	}
	var version int64
	var oldKeys bool
	var targets, partials []byte
	if err := db.pool.QueryRow(ctx, `SELECT state_version,state ? 'leverageTarget' OR state ? 'partialWithdrawal',state->'leverageTargets',state->'partialWithdrawals' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version, &oldKeys, &targets, &partials); err != nil {
		t.Fatal(err)
	}
	if version != 1 || oldKeys {
		t.Fatalf("lease rewrote the version or kept a single-slot key: version %d old %t", version, oldKeys)
	}
	movedTargets, err := decodeLeverageTargets(targets)
	if err != nil || len(movedTargets) != 1 || movedTargets[target.Lane] != target {
		t.Fatalf("stored target was not moved unchanged: %s %v", targets, err)
	}
	movedPartials, err := decodePartialWithdrawals(partials)
	if err != nil || len(movedPartials) != 1 || movedPartials[partial.Lane] != partial {
		t.Fatalf("release sequence was not moved unchanged: %s %v", partials, err)
	}
	if got, err := db.LoadLeverageTarget(ctx, key, onreONycUSDC); err != nil || got != nil {
		t.Fatalf("another lane read AUTO's target: %+v %v", got, err)
	}
	onre := LeverageTarget{Lane: onreONycUSDC, Level: 1.5, SpreadBPS: 120, DecidedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := db.RecordLeverageTarget(ctx, key, onre, 1); err != nil {
		t.Fatal(err)
	}
	for _, want := range []LeverageTarget{target, onre} {
		got, err := db.LoadLeverageTarget(ctx, key, want.Lane)
		if err != nil || got == nil || *got != want {
			t.Fatalf("per-lane target lost: %+v %v", got, err)
		}
	}
	// A second lease acquisition leaves the per-lane maps as they are.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_owner=NULL,lease_expires_at=NULL WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "lane-state", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LoadLeverageTarget(ctx, key, target.Lane); err != nil || got == nil || *got != target {
		t.Fatalf("a later lease changed the per-lane target: %+v %v", got, err)
	}
}
