package referral

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
)

func TestFeeLedgerKeepsInviteeInventorySeparate(t *testing.T) {
	dsn := os.Getenv("REFERRAL_FEE_LEDGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a fresh private referral_fee_ledger_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "referral_fee_ledger_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/") {
		t.Fatal("requires a disposable local socket database")
	}
	ctx := context.Background()
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA loyal_yield")
	for _, name := range []string{"0129_referrals", "0146_referral_wallet_identities", "0147_referral_earn_max_capital", "0148_referral_earn_max_fee_lots"} {
		body, err := os.ReadFile("../../../../migrations/yield/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}
	store := NewStore(db, nil, nil)
	const inviter = "00000000-0000-0000-0000-000000000001"
	const friendA = "00000000-0000-0000-0000-000000000002"
	const friendB = "00000000-0000-0000-0000-000000000003"
	start := time.Unix(1000, 0)
	exec("INSERT INTO loyal_yield.referral_earn_max_program(launch_slot,launched_at) VALUES(100,$1)", start)
	for _, id := range []string{friendA, friendB} {
		exec("INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referee_wallet,referrer_app_user_id,source,referee_signed_up_at,bound_at) VALUES($1,$2,$3,'link',$4,$4)", id, "wallet"+id, inviter, start)
	}
	certify := func(ref string, shares uint64, sig solana.Signature) {
		t.Helper()
		event := earnmax.CapitalMovement{BoundWalletSigned: true, FreeAfter: shares, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: shares, UnixTime: 1050, NetAssetNumerator: "12000", NetAssetDenominator: 1}}}
		r := capitalReplay{At: start.Unix()}
		if err := r.apply(event, start); err != nil {
			t.Fatal(err)
		}
		if err := r.accrue(1300); err != nil {
			t.Fatal(err)
		}
		earned := "0"
		cert := CapitalReport{Coverage: "complete", Slot: 130, At: time.Unix(1300, 0), Earned: &earned}
		if err := store.storeCapital(ctx, ref, "wallet"+ref, "authority"+ref, cert, &r, []capitalFact{{sig, 105, time.Unix(1050, 0), event}}); err != nil {
			t.Fatal(err)
		}
	}
	certify(friendA, 250, solana.Signature{1})
	previous := feeState{Minted: 995, Accrued: 5, OpeningReserve: 5}
	body, _ := json.Marshal(previous)
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_cursor(verified_slot,checked_slot,state) VALUES(100,100,$1)", body)
	block := []solana.Signature{{3}, {4}}
	assessment := feeEvent{FeePoint: FeePoint{Signature: block[1].String(), Slot: 110, Index: 1, At: time.Unix(1100, 0)}, kind: "strategy", fee: 7, supplyBefore: 1000, supplyAfter: 1007}
	harvest := feeEvent{FeePoint: FeePoint{Signature: solana.Signature{5}.String(), Slot: 115, Index: 0, At: time.Unix(1150, 0)}, kind: "harvest", minted: 12}
	observed := previous
	if err := observed.apply(assessment); err != nil {
		t.Fatal(err)
	}
	if err := observed.apply(harvest); err != nil {
		t.Fatal(err)
	}
	save := func(h feeEvent, dry bool) (FeeReport, error) {
		out := FeeReport{}
		err := store.applyFeeBatch(ctx, 100, 120, previous, observed, []feeEvent{assessment, h}, map[uint64][]solana.Signature{110: block}, dry, &out)
		return out, err
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	lots := func(ref string) int {
		return count("SELECT COALESCE(sum(amount_lp_raw),0)::integer FROM loyal_yield.referral_earn_max_fee_lots WHERE referee_app_user_id=$1", ref)
	}
	if _, err := save(harvest, true); err != nil {
		t.Fatal(err)
	}
	if n := count("SELECT count(*) FROM loyal_yield.referral_earn_max_fee_assessments"); n != 0 {
		t.Fatal("dry run wrote an assessment")
	}
	short := harvest
	short.minted--
	if _, err := save(short, false); err == nil {
		t.Fatal("accepted short treasury mint")
	}
	if n := count("SELECT count(*) FROM loyal_yield.referral_earn_max_fee_assessments"); n != 0 {
		t.Fatal("unreconciled assessment escaped rollback")
	}
	out, err := save(harvest, false)
	if err != nil || out.HeldWeights != 1 || out.Lots != 1 || lots(friendA) != 1 || lots(friendB) != 0 {
		t.Fatalf("partial coverage out=%+v err=%v", out, err)
	}
	if _, err := save(harvest, false); err == nil {
		t.Fatal("stale cursor accepted")
	}
	if _, n, err := store.retryFeeWeights(ctx); err != nil || n != 0 {
		t.Fatalf("premature backfill %d, %v", n, err)
	}
	certify(friendB, 500, solana.Signature{2})
	if tried, n, err := store.retryFeeWeights(ctx); err != nil || tried != 1 || n != 1 || lots(friendA) != 1 || lots(friendB) != 3 {
		t.Fatalf("backfill tried=%d lots=%d err=%v", tried, n, err)
	}
	if tried, n, err := store.retryFeeWeights(ctx); err != nil || tried != 0 || n != 0 {
		t.Fatalf("duplicate backfill %d %d %v", tried, n, err)
	}
	var frac string
	if err := db.QueryRow(ctx, "SELECT remainder FROM loyal_yield.referral_earn_max_fee_remainders WHERE referee_app_user_id=$1", friendA).Scan(&frac); err != nil || frac != "3/4" {
		t.Fatalf("friend A carry = %q %v", frac, err)
	}
	whole, rest, err := earnedFeeShares(frac, 1, 1, 4)
	if err != nil || whole.Int64() != 1 || rest != "0" {
		t.Fatalf("carry lost: %s %s %v", whole, rest, err)
	}
}
