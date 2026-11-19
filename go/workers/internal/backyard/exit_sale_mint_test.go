package backyard

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func TestExitReferenceSaleMatchesFinalizedCashAndDebtReceipts(t *testing.T) {
	dsn := os.Getenv("BACKYARD_FOREIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local backyard_foreign_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "backyard_foreign_test") {
		t.Fatal("requires a disposable local backyard_foreign_test database")
	}
	ctx := t.Context()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA loyal_yield;
		CREATE TABLE loyal_yield.multiply_operations (
			operation_id text PRIMARY KEY, route_key text, action text, status text,
			confirmation_status text, confirmed_slot bigint, expected_effects jsonb,
			reconciled_effects jsonb, updated_at timestamptz)`)
	defer exec("DROP SCHEMA loyal_yield CASCADE")

	db := &Database{pool: pool}
	m := RouteManifest{vault: rwa}
	route, err := m.route(autoAUTOWYLDSLane)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_795_000_000, 0).UTC()
	insert := func(id, mint string, slot int64, sold, returned uint64) ExitCost {
		t.Helper()
		output, action := rwa.cash, SwapCollateralToStableStep
		sale := realizedSale{ExpectedSale: ExpectedSale{Lane: route.Lane, SoldRaw: sold, MarkSF: kaminoOne(), MarkValueRaw: sold}, Slot: slot, ReturnedRaw: returned}
		if mint == route.Kamino.DebtMint {
			output, action = route.DebtCustody, SwapCollateralToDebtStep
			sale.Proceeds = &saleProceedsMark{PriceSF: kaminoOne(), USDCSF: kaminoOne(), Decimals: 6}
		}
		sale.OutputAccount = output
		expected := ExpectedEffects{Kind: effectsCrossMintSwap, Sale: &sale.ExpectedSale, Accounts: []ExpectedAccountEffect{
			{Address: route.CollateralCustody, Mint: route.Kamino.CollateralMint, BeforeRaw: sold},
			{Address: output, Mint: mint, AfterRaw: returned},
		}}
		exec(`INSERT INTO loyal_yield.multiply_operations VALUES
			($1,$2,$3,'reconciled','finalized',$4,$5,$6,$7)`, id, rwa.RouteKey, string(action), slot,
			map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "expectedEffects": expected},
			map[string]any{"sale": sale}, at)
		cost, ok := sale.exitCost(at)
		if !ok {
			t.Fatal("invalid sale fixture", id)
		}
		return cost
	}
	check := func(stored, want ExitCost, proceeds string) {
		t.Helper()
		got, err := db.readExitReferenceSale(ctx, m, stored)
		if err != nil || got.ProceedsMint != proceeds || got.Slot != want.Slot ||
			got.QuotedCollateralRaw != want.QuotedCollateralRaw || got.BPS != want.BPS || got.MarkSF != want.MarkSF {
			t.Fatalf("read historical sale: got %+v (%v), want %+v with proceeds %q", got, err, want, proceeds)
		}
	}

	for _, debtReturn := range []uint64{999_900, 999_000} {
		exec("TRUNCATE loyal_yield.multiply_operations")
		stored := insert("latest-debt", route.Kamino.DebtMint, 100, 1_000_000, debtReturn)
		check(stored, stored, route.Kamino.DebtMint)
		insert("older-cash", fleet.USDCMint, 70, 500_000, 499_850)
		cash := insert("cash", fleet.USDCMint, 80, 500_000, 499_750)
		insert("future-cash", fleet.USDCMint, 101, 500_000, 499_000)
		check(stored, cash, fleet.USDCMint)
	}

	exec("TRUNCATE loyal_yield.multiply_operations")
	stored := insert("stored", route.Kamino.DebtMint, 100, 1_000_000, 999_900)
	exec("DELETE FROM loyal_yield.multiply_operations")
	check(stored, stored, "")
	for _, mutation := range []string{
		"route_key='other-vault'",
		"confirmation_status='confirmed'",
		"status='held'",
		"action='REPORT_NAV'",
		"confirmed_slot=99",
		`reconciled_effects=jsonb_set(reconciled_effects,'{sale,lane}','"other"')`,
		`expected_effects=jsonb_set(expected_effects,'{expectedEffects,accounts,0,mint}','"other"')`,
		`expected_effects=jsonb_set(expected_effects,'{expectedEffects,accounts,1,address}','"other"')`,
		`expected_effects=expected_effects #- '{expectedEffects,accounts,1,mint}'`,
		`expected_effects=jsonb_set(expected_effects,'{expectedEffects,accounts,1,mint}','"other"')`,
	} {
		insert("invalid", route.Kamino.DebtMint, 100, 1_000_000, 999_900)
		exec("UPDATE loyal_yield.multiply_operations SET " + mutation)
		check(stored, stored, "")
		exec("DELETE FROM loyal_yield.multiply_operations")
	}
	insert("earlier-debt", route.Kamino.DebtMint, 99, 1_000_000, 999_900)
	insert("different-lot", route.Kamino.DebtMint, 100, 2_000_000, 1_999_800)
	check(stored, stored, "")
}
