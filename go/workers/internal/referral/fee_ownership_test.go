package referral

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
)

func TestReferralFeeOwnersUsePreAssessmentFinalizedOrder(t *testing.T) {
	start := time.Unix(1000, 0)
	block := []solana.Signature{{1}, {2}, {3}}
	p := FeePoint{Signature: block[1].String(), Slot: 110, Index: 1, At: time.Unix(1100, 0)}
	order, _, err := feeBlockOrder(p, block)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := feeBlockOrder(p, []solana.Signature{block[0], block[0]}); err == nil {
		t.Fatal("accepted incorrect block membership")
	}
	deposit := earnmax.CapitalMovement{BoundWalletSigned: true, FreeAfter: 100, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: 100, UnixTime: 1010, NetAssetNumerator: "9950", NetAssetDenominator: 1}}}
	request := earnmax.CapitalMovement{FreeBefore: 100, FreeAfter: 60, EscrowAfter: 40, Events: []earnmax.CapitalEvent{{Kind: "request", LP: 40, UnixTime: 1100}}}
	claim := earnmax.CapitalMovement{FreeBefore: 60, FreeAfter: 60, EscrowBefore: 40, Events: []earnmax.CapitalEvent{{Kind: "claim", LP: 40, UnixTime: 1110}}}
	through := time.Unix(1150, 0)
	var expected capitalReplay
	expected.At = start.Unix()
	for _, m := range []earnmax.CapitalMovement{deposit, request, claim} {
		if err := expected.apply(m, start); err != nil {
			t.Fatal(err)
		}
	}
	if err := expected.accrue(through.Unix()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sig  solana.Signature
		want uint64
		hold bool
	}{
		{"accepted before assessment excludes reserved shares", block[0], 60, false},
		{"accepted later in the same block leaves shares eligible", block[2], 100, false},
		{"capital event in same transaction holds the period", block[1], 0, true},
		{"absent from supplied finalized block holds the period", solana.Signature{9}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := feeOwnership{start: start, throughAt: through, expected: expected, replay: capitalReplay{At: start.Unix()}}
			if err := o.apply(p, order, 101, solana.Signature{4}.String(), deposit); err != nil {
				t.Fatal(err)
			}
			err := o.apply(p, order, 110, tc.sig.String(), request)
			if tc.hold {
				if err == nil {
					t.Fatal("unproved ordering accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = o.apply(p, order, 111, solana.Signature{5}.String(), claim); err != nil {
				t.Fatal(err)
			}
			owner, err := o.finish()
			if err != nil || owner.Shares != tc.want {
				t.Fatalf("shares=%d err=%v", owner.Shares, err)
			}
		})
	}
}

func TestReferralFeeOwnersRequireCompleteHistoricalCapital(t *testing.T) {
	dsn := os.Getenv("REFERRAL_FEE_OWNER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a private local referral_fee_owner_test PostgreSQL fixture")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "referral_fee_owner_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/") {
		t.Fatal("requires a private local owner test database")
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
	for _, name := range []string{"0129_referrals", "0146_referral_wallet_identities", "0147_referral_earn_max_capital"} {
		body, err := os.ReadFile("../../../../migrations/yield/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}
	store := NewStore(db, nil, nil)
	const referee = "00000000-0000-0000-0000-000000000001"
	const inviter = "00000000-0000-0000-0000-000000000002"
	const after = "00000000-0000-0000-0000-000000000003"
	launch := time.Unix(1000, 0)
	exec("INSERT INTO loyal_yield.referral_earn_max_program(launch_slot,launched_at) VALUES(100,$1)", launch)
	bind := func(id, w string, bound time.Time) {
		t.Helper()
		exec("INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referee_wallet,referrer_app_user_id,source,referee_signed_up_at,bound_at) VALUES($1,$2,$3,'link',$4,$4)", id, w, inviter, bound)
	}
	bind(referee, "wallet", launch)
	bind(after, "future", time.Unix(2000, 0))
	block := []solana.Signature{{1}, {2}}
	point := FeePoint{Signature: block[1].String(), Index: 1, Slot: 110, At: time.Unix(1100, 0)}
	read := func() (feeOwner, error) {
		tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return feeOwnerForTx(ctx, tx, point, block, referee)
	}
	if _, err := read(); !errors.Is(err, ErrFeeOwnershipUnproven) {
		t.Fatal("missing capital status was counted as zero")
	}
	m := earnmax.CapitalMovement{BoundWalletSigned: true, FreeAfter: 100, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: 100, UnixTime: 1100, NetAssetNumerator: "9950", NetAssetDenominator: 1}}}
	checkpoint := time.Unix(1200, 0)
	r := capitalReplay{At: launch.Unix()}
	if err = r.apply(m, launch); err != nil {
		t.Fatal(err)
	}
	if err = r.accrue(checkpoint.Unix()); err != nil {
		t.Fatal(err)
	}
	value := "0"
	cert := CapitalReport{Coverage: "complete", Slot: 120, At: checkpoint, Earned: &value}
	f := capitalFact{block[0], point.Slot, point.At, m}
	if err := store.storeCapital(ctx, referee, "wallet", "authority", cert, &r, []capitalFact{f}); err != nil {
		t.Fatal(err)
	}
	// A later gap does not erase independently certified ownership at point.
	if err := store.storeCapital(ctx, referee, "wallet", "authority", CapitalReport{Coverage: "incomplete", Reason: "history_page_limit", Slot: 130, At: time.Unix(1300, 0)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	owners, err := read()
	if err != nil || owners != (feeOwner{referee, inviter, 100}) {
		t.Fatalf("owners=%+v err=%v", owners, err)
	}
	// Another ordinary invited signup may never open a smart account. Its
	// independently deferred claim must leave this verified referee unchanged.
	const missing = "00000000-0000-0000-0000-000000000004"
	bind(missing, "unmapped", launch)
	if owner, err := read(); err != nil || owner.Shares != 100 {
		t.Fatalf("unrelated missing referral blocked eligible capital: %+v %v", owner, err)
	}
	beforePoint := point
	priorBody, err := json.Marshal(&r)
	if err != nil {
		t.Fatal(err)
	}
	zeroAt := time.Unix(1400, 0)
	resumed := capitalReplay{At: zeroAt.Unix(), LastEventAt: zeroAt.Unix()}
	resumed.Earned.Set(&r.Earned)
	anchorBody, err := json.Marshal(&resumed)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO loyal_yield.referral_earn_max_capital_resets(referee_app_user_id,authority,prior_through_slot,prior_through_at,prior_replay,zero_slot,zero_at,zero_replay,reason)
	VALUES($1,'authority',120,$2,$3,140,$4,$5,'mixed_share_origin')`, referee, checkpoint, priorBody, zeroAt, anchorBody)
	newDeposit := earnmax.CapitalMovement{BoundWalletSigned: true, FreeAfter: 35, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: 35, NetAssetNumerator: "300", NetAssetDenominator: 1, UnixTime: 1500}}}
	if err = resumed.apply(newDeposit, launch); err != nil {
		t.Fatal(err)
	}
	finalAt := time.Unix(1600, 0)
	if err = resumed.accrue(finalAt.Unix()); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(&resumed)
	if err != nil {
		t.Fatal(err)
	}
	factBody, err := json.Marshal(newDeposit)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO loyal_yield.referral_earn_max_capital_facts(referee_app_user_id,authority,slot,signature,block_time,movement) VALUES($1,'authority',150,$2,$3,$4)`, referee, (solana.Signature{8}).String(), time.Unix(1500, 0), factBody)
	exec(`UPDATE loyal_yield.referral_earn_max_capital_status SET as_of_slot=160,as_of_at=$2,earned_through_slot=160,earned_through_at=$2,replay=$3,coverage='incomplete',reason='historical_gap' WHERE referee_app_user_id=$1`, referee, finalAt, body)
	if o, err := read(); err != nil || o.Shares != 100 {
		t.Fatalf("previous independent interval %v %+v", err, o)
	}
	point.Slot, point.At = 140, zeroAt
	if _, err = read(); !errors.Is(err, ErrFeeOwnershipUnproven) {
		t.Fatalf("end-of-slot zero certified an earlier intrablock point: %v", err)
	}
	point.Slot, point.At = 135, time.Unix(1350, 0)
	if _, err = read(); !errors.Is(err, ErrFeeOwnershipUnproven) {
		t.Fatalf("fee point inside a gap was certified: %v", err)
	}
	point.Slot, point.At = 151, time.Unix(1510, 0)
	if o, err := read(); err != nil || o.Shares != 35 {
		t.Fatalf("resumed independent interval %v %+v", err, o)
	}
	point = beforePoint
	exec("INSERT INTO loyal_yield.referral_wallet_identities(wallet,privy_subject,app_user_id) VALUES ('wallet','x',$1),('inviter','x',$2)", referee, inviter)
	if _, err := read(); !errors.Is(err, ErrFeeOwnershipUnproven) {
		t.Fatal("later proof of self-referral did not hold funding")
	}
	exec("DELETE FROM loyal_yield.referral_wallet_identities")
	exec("DELETE FROM loyal_yield.referral_earn_max_capital_facts")
	if _, err := read(); !errors.Is(err, ErrFeeOwnershipUnproven) {
		t.Fatal("missing evidence credited an invented zero share count")
	}
}
