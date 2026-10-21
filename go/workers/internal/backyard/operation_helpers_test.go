package backyard

import (
	"context"
	"errors"
	"testing"
)

// assertPreSendHold wants a hold whose reason (one of the enum) or cause is
// reason.
func assertPreSendHold(t *testing.T, err error, reason string) {
	t.Helper()
	var hold *PreSendHold
	if !errors.As(err, &hold) || string(hold.Reason) != reason && holdCause(hold) != reason {
		t.Fatalf("wanted HOLD %s; got %v", reason, err)
	}
}

func operationStatus(t *testing.T, ctx context.Context, db *Database, id string) string {
	t.Helper()
	var status string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// seedDecidedOperation seeds one decided operation on a test-owned route key
// with expected_effects as given, so cleanup deletes exactly these rows.
func seedDecidedOperation(t *testing.T, ctx context.Context, db *Database, key string, action Action, lane string, effects []byte) string {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	id := key + "-op"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, string(action), lane, string(effects)); err != nil {
		t.Fatal(err)
	}
	return id
}
