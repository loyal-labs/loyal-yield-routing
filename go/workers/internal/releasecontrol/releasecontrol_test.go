package releasecontrol

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

func TestReleaseControlDefaultDenyCASAndAdmissionFence(t *testing.T) {
	dsn := os.Getenv("BACKYARD_WITHDRAWAL_TEST_DSN")
	if dsn == "" {
		t.Skip("explicit disposable release control database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "vault_cost_test") {
		t.Fatal("release control database must be explicitly disposable localhost vault_cost_test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE loyal_yield.vault_release_controls`); err != nil {
		t.Fatal(err)
	}
	vault, owner, other := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	check := func(owners []string, amount uint64) (Admission, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return Check(ctx, tx, vault, OrdinaryFunding, owners, &amount)
	}
	refused := func(err error) string {
		var r *Refusal
		if errors.As(err, &r) {
			return r.Reason
		}
		return ""
	}
	if _, err := check([]string{owner}, 1); refused(err) != RefusedHeld {
		t.Fatalf("missing control admitted funding: %v", err)
	}
	cap := uint64(100)
	open := Change{Vault: vault, Action: OrdinaryFunding, State: Open, Reason: "canary", Operator: "ops", AllowedOwners: []string{owner}, MaxAmountRaw: &cap}
	if _, err := Set(ctx, pool, Change{Vault: vault, Action: OptionalAddition, State: Open, Reason: "x", Operator: "ops", MaxAmountRaw: &cap}); err == nil {
		t.Fatal("cap accepted on an action without an amount")
	}
	if c, err := Set(ctx, pool, Change{Vault: vault, Action: OptionalAddition, State: Open, Reason: "x", Operator: "ops"}); err != nil || c.Version != 1 {
		t.Fatalf("optional additions could not be opened: %+v %v", c, err)
	}
	c, err := Set(ctx, pool, open)
	if err != nil || c.Version != 1 {
		t.Fatalf("create: %+v %v", c, err)
	}
	for _, expect := range []int64{0, 5} {
		stale := open
		stale.ExpectVersion = expect
		if _, err := Set(ctx, pool, stale); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("stale version %d overwrote the control: %v", expect, err)
		}
	}
	if _, err := check([]string{owner, other}, 1); refused(err) != RefusedOwner {
		t.Fatalf("unlisted owner admitted: %v", err)
	}
	if _, err := check([]string{owner}, 101); refused(err) != RefusedLimit {
		t.Fatalf("over-cap amount admitted: %v", err)
	}
	if a, err := check([]string{owner}, 100); err != nil || a.Version != 1 {
		t.Fatalf("admitted owner refused: %+v %v", a, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(ctx, tx, vault, OrdinaryFunding, []string{owner}, &cap); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Set(ctx, pool, Change{Vault: vault, Action: OrdinaryFunding, State: Held, ExpectVersion: 1, Reason: "stop", Operator: "ops"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("hold committed under an admitting transaction: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := check([]string{owner}, 1); refused(err) != RefusedHeld {
		t.Fatalf("held control admitted funding: %v", err)
	}
	var versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.vault_release_control_history WHERE vault=$1 AND action=$2`, vault, string(OrdinaryFunding)).Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("history lost a version: %d %v", versions, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM loyal_yield.vault_release_control_history WHERE vault=$1`, vault); err == nil {
		t.Fatal("history rewritten")
	}
}
