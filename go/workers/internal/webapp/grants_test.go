package webapp

import (
	"context"
	"testing"
	"time"
)

func TestMissingGrantsChecksASequenceAsASequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool := earningsLockTestPool(t, ctx)
	if _, err := pool.Exec(ctx, `CREATE SEQUENCE IF NOT EXISTS public.grant_check_test_event_id_seq`); err != nil {
		t.Fatal(err)
	}

	missing, err := MissingGrants(ctx, pool, []Grant{{"public.grant_check_test_event_id_seq", "USAGE"}})

	if err != nil || len(missing) != 0 {
		t.Fatalf("missing=%v err=%v, want USAGE on the sequence its owner holds", missing, err)
	}
}
