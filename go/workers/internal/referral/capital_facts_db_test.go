package referral

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
)

func TestReferralCapitalCheckpointIsMonotonicAndRetainsProvenEarnings(t *testing.T) {
	dsn := os.Getenv("REFERRAL_CAPITAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a private local PostgreSQL fixture")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "referral_capital_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/refcap-postgres.") {
		t.Fatal("requires an isolated local referral capital test database")
	}
	ctx := context.Background()
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA loyal_yield")
	for _, name := range []string{"0129_referrals.sql", "0146_referral_wallet_identities.sql", "0147_referral_earn_max_capital.sql"} {
		body, err := os.ReadFile(filepath.Join("../../../../migrations/yield", name))
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}
	const id = "00000000-0000-0000-0000-000000000001"
	const auth = "account"
	exec("INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referee_wallet,referrer_app_user_id,source,referee_signed_up_at) VALUES($1,'wallet','00000000-0000-0000-0000-000000000002','link',now())", id)
	store := NewStore(db, nil, nil)
	fact := capitalFact{solana.Signature{1}, 105, time.Unix(105, 0), earnmax.CapitalMovement{FreeAfter: 2}}
	save := func(slot uint64, earned, reason string, f capitalFact, account string) error {
		out := CapitalReport{Coverage: "incomplete", At: time.Unix(int64(slot), 0), Slot: slot, Reason: reason}
		state := capitalReplay{At: out.At.Unix()}
		if earned != "" {
			out.Coverage = "complete"
			out.Earned = &earned
			state.Earned.SetString(earned)
		}
		return store.storeCapital(ctx, id, "wallet", account, out, &state, []capitalFact{f})
	}
	mustSave := func(slot uint64, earned, reason string) {
		t.Helper()
		if err := save(slot, earned, reason, fact, auth); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(110, "12", "")
	mustSave(120, "", "history_not_retained")
	mustSave(115, "15", "")
	check := func(slot, earnedSlot int, earned, coverage string) {
		t.Helper()
		var s, es, count int
		var e, c string
		if err := db.QueryRow(ctx, "SELECT as_of_slot,earned_through_slot,earned_micro_usdc::text,coverage,(SELECT COUNT(*) FROM loyal_yield.referral_earn_max_capital_facts) FROM loyal_yield.referral_earn_max_capital_status WHERE referee_app_user_id=$1", id).Scan(&s, &es, &e, &c, &count); err != nil {
			t.Fatal(err)
		}
		if s != slot || es != earnedSlot || e != earned || c != coverage || count != 1 {
			t.Fatalf("slot=%d earnedThrough=%d earned=%s coverage=%s facts=%d", s, es, e, c, count)
		}
	}
	check(120, 115, "15", "incomplete")
	mustSave(120, "17", "")
	mustSave(120, "", "history_not_retained")
	mustSave(120, "17", "")
	check(120, 120, "17", "complete")
	forged := fact
	forged.movement.FreeAfter = 22
	if err := save(130, "30", "", forged, auth); err == nil {
		t.Fatal("rewrote finalized evidence")
	}
	if err := save(120, "33", "", fact, auth); err == nil {
		t.Fatal("rewrote the same finalized checkpoint")
	}
	if err := save(130, "30", "", fact, "different authority"); err == nil {
		t.Fatal("changed the verified account")
	}
	exec("INSERT INTO loyal_yield.referral_wallet_identities(wallet,privy_subject,app_user_id) VALUES ('wallet','same-identity',$1),('other-wallet','same-identity','00000000-0000-0000-0000-000000000002')", id)
	if err := save(140, "40", "", fact, auth); err == nil {
		t.Fatal("credited a new identity conflict")
	}
	mustSave(140, "", "attribution_conflict")
	check(140, 120, "17", "incomplete")
	t.Run("zero reset preserves earlier checkpoint and rejects stale concurrent recovery", func(t *testing.T) {
		exec("DELETE FROM loyal_yield.referral_wallet_identities")
		var prior []byte
		var priorSlot uint64
		var priorAt time.Time
		if err := db.QueryRow(ctx, "SELECT replay,earned_through_slot,earned_through_at FROM loyal_yield.referral_earn_max_capital_status WHERE referee_app_user_id=$1", id).Scan(&prior, &priorSlot, &priorAt); err != nil {
			t.Fatal(err)
		}
		value := "17"
		zeroAt := time.Unix(145, 0).UTC()
		tmp := capitalReplay{At: zeroAt.Unix(), LastEventAt: zeroAt.Unix()}
		tmp.Earned.SetInt64(17)
		raw, err := json.Marshal(&tmp)
		if err != nil {
			t.Fatal(err)
		}
		reset := &capitalReset{priorSlot: &priorSlot, priorAt: &priorAt, priorReplay: prior, slot: 145, at: zeroAt, replay: raw, reason: "mixed_share_origin"}
		out := CapitalReport{Slot: 145, At: zeroAt, Coverage: "incomplete", Reason: "historical_gap", Earned: &value, checkedReset: true, reset: reset}
		if err := store.storeCapital(ctx, id, "wallet", auth, out, &tmp, nil); err != nil {
			t.Fatal(err)
		}
		check(145, 145, "17", "incomplete")
		var begin, end int
		if err := db.QueryRow(ctx, "SELECT prior_through_slot,zero_slot FROM loyal_yield.referral_earn_max_capital_resets WHERE referee_app_user_id=$1", id).Scan(&begin, &end); err != nil || begin != 120 || end != 145 {
			t.Fatalf("gap=(%d,%d] err=%v", begin, end, err)
		}
		if err := store.storeCapital(ctx, id, "wallet", auth, out, &tmp, nil); err == nil {
			t.Fatal("stale concurrent reset admitted")
		}
	})
	if ids, active, err := store.capitalBatchIDs(ctx, false); err != nil || active || len(ids) != 0 {
		t.Fatalf("unlaunched batch %+v active=%v err=%v", ids, active, err)
	}
	exec("INSERT INTO loyal_yield.referral_earn_max_program(launch_slot,launched_at) VALUES(100,now())")
	t.Run("round robin includes failed or unmapped referrals without starving the next page", func(t *testing.T) {
		exec(`INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referee_wallet,referrer_app_user_id,source,referee_signed_up_at) SELECT md5(i::text)::uuid, 'wallet'||i,'00000000-0000-0000-0000-000000000002','link',now() FROM generate_series(1,5) i`)
		next := func(dry bool) []string {
			t.Helper()
			rows, active, err := store.capitalBatchIDs(ctx, dry)
			if !active || err != nil {
				t.Fatalf("active=%v err=%v", active, err)
			}
			return rows
		}
		preview := next(true)
		first := next(false)
		second := next(false)
		again := next(false)
		if len(first) != 4 || len(second) != 2 || !slices.Equal(first, preview) || !slices.Equal(first, again) {
			t.Fatalf("pages %v,%v,%v preview %v", first, second, again, preview)
		}
		for _, id := range second {
			if slices.Contains(first, id) {
				t.Fatal("attempted the same referral twice before finishing a cycle")
			}
		}
	})
	if _, err := db.Exec(ctx, "UPDATE loyal_yield.referral_earn_max_program SET launch_slot=50"); err == nil {
		t.Fatal("launch allowed mutation")
	}
}
