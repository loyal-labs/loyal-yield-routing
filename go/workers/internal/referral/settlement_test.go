package referral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func TestSettlementIntegerCaps(t *testing.T) {
	for _, c := range []struct {
		due, available, supply, value int64
		lp, paid                      uint64
	}{{15_000_000, 9_000_000, 1, 1, 9_000_000, 9_000_000}, {12, 100, 3, 5, 7, 12}, {12, 2, 3, 5, 2, 4}, {1, 20, 3, 5, 0, 0}, {0, 100, 1, 1, 0, 0}} {
		lp, paid, err := settlementSize(big.NewInt(c.due), big.NewInt(c.available), big.NewInt(c.supply), uint64(c.value))
		if lp != c.lp || (err != nil) != (c.lp == 0) || (err == nil && paid.Uint64() != c.paid) {
			t.Fatalf("size %+v: %d %v %v", c, lp, paid, err)
		}
	}
	now := time.Now()
	if quoteCurrent(133, now, 100, now) || quoteCurrent(101, now.Add(31*time.Second), 100, now) || !quoteCurrent(101, now.Add(time.Second), 100, now) {
		t.Fatal("invalid signing deadline")
	}
}

func signSettlementTestWire(t *testing.T, wire []byte, key solana.PrivateKey) (*solana.Transaction, []byte) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k == key.PublicKey() {
			return &key
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return tx, signed
}

// Exercise the real RPC codec, exact signed wire, SQL migrations and operator
// entrypoints on an isolated database. No signing key or network send exists in
// the implementation; this fixture signs with a random disposable key.
func TestSettlementLifecycle(t *testing.T) {
	dsn := os.Getenv("REFERRAL_SETTLEMENT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("private local PostgreSQL fixture required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "referral_settlement_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/refsettle-postgres.") {
		t.Fatal("requires its own local database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA loyal_yield")
	for _, m := range []string{"0129_referrals.sql", "0146_referral_wallet_identities.sql", "0147_referral_earn_max_capital.sql", "0148_referral_earn_max_fee_lots.sql", "0149_referral_earn_max_settlements.sql"} {
		body, err := os.ReadFile(filepath.Join("../../../../migrations/yield", m))
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}

	const inviter = "00000000-0000-4000-8000-000000000001"
	const friend = "00000000-0000-4000-8000-000000000002"
	const other = "00000000-0000-4000-8000-000000000003"
	// Public Squads SDK settings fixture used by the protocol decoder tests.
	settingsData, err := base64.StdEncoding.DecodeString("37OjvrHgQ60KHQIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/AQAAAJcaJGJKF/Wed7NyrW3KxJvGybD8BjcCj1JwIF/Ul3wIBwABAgEAAAAAAAAArW3KxJvGybD8BjcCj1JwIF/Ul3wIBwABQAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := squads.DecodeSettings(&chain.Account{Owner: squads.ProgramID, Data: settingsData})
	if err != nil {
		t.Fatal(err)
	}
	settingsPK, _, err := squads.SettingsAddress(settings.Seed)
	if err != nil {
		t.Fatal(err)
	}
	treasuryKey := solana.NewWallet().PrivateKey
	treasury := treasuryKey.PublicKey()
	source, err := spl.AssociatedTokenAddress(treasury, earnmax.LPMint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	exec("CREATE TABLE app_users(id uuid PRIMARY KEY,provider text,subject_address text); CREATE TABLE app_user_smart_accounts(user_id uuid,solana_env text,state text,settings_pda text)")
	exec("INSERT INTO app_users VALUES($1,'solana',$2)", inviter, settings.Signers[0].Key.String())
	exec("INSERT INTO app_user_smart_accounts VALUES($1,'mainnet','ready',$2)", inviter, settingsPK.String())
	exec("INSERT INTO loyal_yield.referral_earn_max_program(launch_slot,launched_at) VALUES(50,now())")
	for i, id := range []string{friend, other} {
		exec("INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referrer_app_user_id,referee_wallet,source,referee_signed_up_at) VALUES($1,$2,$3,'link',now())", id, inviter, fmt.Sprintf("wallet%d", i))
		exec("INSERT INTO loyal_yield.referral_earn_max_capital_status VALUES($1,'authority',91,now(),'incomplete','later gap',15000000,90,now(),'{}')", id)
	}
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_harvests VALUES('harvest',80,1,$1,29000000)", source.String())
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_assessments VALUES('assess',70,0,29000000,29000000,now(),ARRAY['assess'],'harvest')")
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_weights(assessment_signature,referee_app_user_id,referrer_app_user_id,shares_raw,realized) VALUES('assess',$1,$3,9000000,true),('assess',$2,$3,20000000,true)", friend, other, inviter)
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_harvests VALUES('second-harvest',81,0,$1,2000)", source.String())
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_assessments VALUES('fraction',71,0,100000,2000,now(),ARRAY['fraction'],'second-harvest')")
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_weights(assessment_signature,referee_app_user_id,referrer_app_user_id,hold_reason) VALUES('fraction',$1,$2,'unknown')", friend, inviter)
	lot := func(id string, amt int) int64 {
		t.Helper()
		var lot int64
		err := pool.QueryRow(ctx, "INSERT INTO loyal_yield.referral_earn_max_fee_lots(referee_app_user_id,referrer_app_user_id,assessment_signature,assessment_slot,harvest_signature,harvest_slot,vault,lp_mint,treasury_lp_account,amount_lp_raw) VALUES($1,$2,'assess',70,'harvest',80,$3,$4,$5,$6) RETURNING lot_id", id, inviter, earnmax.Vault.String(), earnmax.LPMint.String(), source.String(), amt).Scan(&lot)
		if err != nil {
			t.Fatal(err)
		}
		return lot
	}
	lot1, lot2 := lot(friend, 9_000_000), lot(other, 20_000_000)
	var wire []byte
	var landed *solana.Transaction
	var fixturePlan Settlement
	var expired, missingHistory, withLanding, badCredit, failed bool
	at := time.Now().UTC().Unix()
	acct := func(owner solana.PublicKey, b []byte) map[string]any {
		return map[string]any{"data": []any{base64.StdEncoding.EncodeToString(b), "base64"}, "executable": false, "lamports": 10, "owner": owner.String(), "rentEpoch": 0}
	}
	put := binary.LittleEndian.PutUint64
	clock := make([]byte, 40)
	put(clock, 100)
	put(clock[32:], uint64(at))
	vault := make([]byte, voltr.VaultSize)
	copy(vault, voltr.VaultDiscriminator[:])
	copy(vault[104:], earnmax.AssetMint[:])
	copy(vault[272:], earnmax.LPMint[:])
	copy(vault[400:], treasury[:])
	put(vault[168:], 90_000_000)
	put(vault[680:], uint64(at-10))
	mint := make([]byte, 82)
	put(mint[36:], 90_000_000)
	mint[44], mint[45] = 6, 1
	src := make([]byte, 165)
	copy(src, earnmax.LPMint[:])
	copy(src[32:], treasury[:])
	put(src[64:], 29_000_000)
	src[108] = 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		wrap := func(v any) any { return map[string]any{"context": map[string]any{"slot": 100}, "value": v} }
		switch req.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
		case "getMultipleAccounts":
			result = wrap([]any{acct(solana.SysVarPubkey, clock), acct(voltr.ProgramID, vault), acct(solana.TokenProgramID, mint), acct(squads.ProgramID, settingsData), acct(solana.TokenProgramID, src), nil})
		case "getLatestBlockhash":
			result = wrap(map[string]any{"blockhash": solana.Hash{2}.String(), "lastValidBlockHeight": 200})
		case "isBlockhashValid":
			result = wrap(true)
			if expired {
				result = map[string]any{"context": map[string]any{"slot": 220}, "value": false}
			}
		case "getEpochInfo":
			result = map[string]any{"absoluteSlot": 220, "blockHeight": 210}
		case "getFirstAvailableBlock":
			result = 10
		case "getBlockTime":
			result = at + 60
		case "getSignaturesForAddress":
			rows := []any{}
			if withLanding {
				rows = append(rows, map[string]any{"signature": landed.Signatures[0].String(), "slot": 102, "err": nil})
			}
			if !missingHistory {
				rows = append(rows, map[string]any{"signature": solana.Signature{4}.String(), "slot": 80, "err": nil})
			}
			result = rows
		case "getTransaction":
			if !withLanding {
				result = nil
				break
			}
			index := func(a string) int {
				for i, k := range landed.Message.AccountKeys {
					if k.String() == a {
						return i
					}
				}
				return -1
			}
			bal := func(address, owner string, amount uint64) any {
				return map[string]any{"accountIndex": index(address), "mint": earnmax.LPMint.String(), "owner": owner, "programId": solana.TokenProgramID.String(), "uiTokenAmount": map[string]any{"amount": strconv.FormatUint(amount, 10), "decimals": 6}}
			}
			credit := fixturePlan.LPRaw
			if badCredit {
				credit--
			}
			var executionErr any
			if failed {
				executionErr = "AccountInUse"
			}
			result = map[string]any{"slot": 102, "blockTime": at + 1, "transaction": []any{base64.StdEncoding.EncodeToString(wire), "base64"}, "meta": map[string]any{"err": executionErr, "preTokenBalances": []any{bal(source.String(), treasury.String(), 29_000_000)}, "postTokenBalances": []any{bal(source.String(), treasury.String(), 29_000_000-fixturePlan.LPRaw), bal(fixturePlan.RecipientATA, fixturePlan.RecipientOwner, credit)}}}
		default:
			t.Errorf("unexpected RPC method %s", req.Method)
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	client, err := chain.New(server.URL, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool, pool, nil)
	put(src[64:], 15_000_000)
	if _, err := s.PrepareSettlement(ctx, client, lot1); err == nil {
		t.Fatal("used another friend’s LP during a shared treasury shortfall")
	}
	put(src[64:], 29_000_001)
	if _, err := s.PrepareSettlement(ctx, client, lot1); err == nil {
		t.Fatal("spent collateral for an unresolved harvested assessment")
	}
	exec("UPDATE loyal_yield.referral_earn_max_fee_weights SET shares_raw=1,hold_reason=NULL,realized=true WHERE assessment_signature='fraction'")
	put(src[64:], 29_000_000)
	if _, err := s.PrepareSettlement(ctx, client, lot1); err == nil {
		t.Fatal("spent another settlement's fractional LP backing")
	}
	put(src[64:], 29_000_001)
	type prepared struct {
		p   Settlement
		err error
	}
	results := make(chan prepared, 2)
	for range 2 {
		go func() { p, err := s.PrepareSettlement(ctx, client, lot1); results <- prepared{p, err} }()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.p.ID != second.p.ID {
		t.Fatalf("concurrent preparations: %v %v %s %s", first.err, second.err, first.p.ID, second.p.ID)
	}
	p1 := first.p
	if p1.LPRaw != 9_000_000 || p1.MicroUSDC != "9000000" {
		t.Fatalf("wrong payout %+v", p1)
	}
	again, err := s.PrepareSettlement(ctx, client, lot1)
	if err != nil || again.ID != p1.ID || !bytes.Equal(p1.UnsignedWire, again.UnsignedWire) {
		t.Fatalf("duplicate preparation %v", err)
	}
	// The second friend shares the inviter and treasury, but draws from its own lot.
	p2, err := s.PrepareSettlement(ctx, client, lot2)
	if err != nil || p2.LPRaw != 15_000_000 {
		t.Fatalf("second reservation %v %d", err, p2.LPRaw)
	}
	tx2, signed2 := signSettlementTestWire(t, p2.UnsignedWire, treasuryKey)
	if _, err = s.RegisterSettlement(ctx, client, p1.ID, signed2); err == nil {
		t.Fatal("accepted another friend's signed transaction")
	}
	landed, wire = signSettlementTestWire(t, p1.UnsignedWire, treasuryKey)
	fixturePlan = p1
	if len(landed.Message.Instructions) != 3 || landed.Message.AccountKeys[landed.Message.Instructions[1].ProgramIDIndex] != solana.TokenProgramID || !bytes.Contains(landed.Message.Instructions[2].Data, []byte(p1.ID)) {
		t.Fatal("invalid payout transaction shape")
	}
	signedPlan, err := s.RegisterSettlement(ctx, client, p1.ID, wire)
	if err != nil || signedPlan.State != "signed" || !signedPlan.NewRegistration {
		t.Fatalf("register %v", err)
	}
	if duplicate, err := s.RegisterSettlement(ctx, client, p1.ID, wire); err != nil || duplicate.NewRegistration {
		t.Fatalf("duplicate authorization %v", err)
	}
	if _, err = s.ReconcileSettlement(ctx, client, p1.ID); err == nil {
		t.Fatal("unknown transaction should stay reserved")
	}
	withLanding, badCredit = true, true
	if _, err = s.ReconcileSettlement(ctx, client, p1.ID); err == nil {
		t.Fatal("accepted mismatched balance delta")
	}
	badCredit = false
	done, err := s.ReconcileSettlement(ctx, client, p1.ID)
	if err != nil || done.State != "delivered" {
		t.Fatalf("delivery %v", err)
	}
	paid := func(id string) (string, bool) {
		t.Helper()
		var amount string
		var open bool
		if err := pool.QueryRow(ctx, "SELECT paid_micro_usdc::text,has_open_settlement FROM loyal_yield.referral_earn_max_paid WHERE referee_app_user_id=$1", id).Scan(&amount, &open); err != nil {
			t.Fatal(err)
		}
		return amount, open
	}
	if amount, open := paid(friend); amount != "9000000" || open {
		t.Fatalf("paid %s open %t", amount, open)
	}
	if amount, open := paid(other); amount != "0" || !open {
		t.Fatalf("second paid %s open %t", amount, open)
	}
	if _, err = s.PrepareSettlement(ctx, client, lot1); err == nil {
		t.Fatal("depleted lot funded outstanding $6")
	}
	// Incomplete archive cannot free an exported draft, including after expiry.
	expired, withLanding, missingHistory = true, false, true
	if _, err = s.RecoverSettlement(ctx, client, p2.ID, solana.Signature{9}); err == nil {
		t.Fatal("unverified history cursor released the hold")
	}
	if _, err = s.RecoverSettlement(ctx, client, p2.ID, solana.Signature{}); err == nil {
		t.Fatal("trusted incomplete archive")
	}
	missingHistory = false
	abandoned, err := s.RecoverSettlement(ctx, client, p2.ID, solana.Signature{})
	if err != nil || abandoned.State != "expired" {
		t.Fatalf("expire %v", err)
	}
	expired = false
	again, err = s.PrepareSettlement(ctx, client, lot2)
	if err != nil || again.ID == p2.ID {
		t.Fatalf("replacement %v", err)
	}
	// A registered but unbroadcast wire can also expire safely.
	_, signedRetry := signSettlementTestWire(t, again.UnsignedWire, treasuryKey)
	if _, err = s.RegisterSettlement(ctx, client, again.ID, signedRetry); err != nil {
		t.Fatal(err)
	}
	expired = true
	signedExpiry, err := s.RecoverSettlement(ctx, client, again.ID, solana.Signature{})
	if err != nil || signedExpiry.State != "expired" || signedExpiry.Signature == nil {
		t.Fatalf("signed expiry: %v", err)
	}
	expired = false
	again, err = s.PrepareSettlement(ctx, client, lot2)
	if err != nil {
		t.Fatal(err)
	}
	landed, wire = signSettlementTestWire(t, again.UnsignedWire, treasuryKey)
	fixturePlan = again
	if _, err = s.RegisterSettlement(ctx, client, again.ID, wire); err != nil {
		t.Fatal(err)
	}
	withLanding, failed = true, true
	rejected, err := s.ReconcileSettlement(ctx, client, again.ID)
	if err != nil || rejected.State != "failed" {
		t.Fatalf("finalized failure: %v", err)
	}
	if amount, open := paid(other); amount != "0" || open {
		t.Fatalf("failed transfer paid %s held %t", amount, open)
	}
	failed = false
	again, err = s.PrepareSettlement(ctx, client, lot2)
	if err != nil {
		t.Fatal(err)
	}
	// A signer submitted a draft outside the normal registration flow. Archive
	// discovery identifies its exact message and books its original fixed quote.
	landed, wire = signSettlementTestWire(t, again.UnsignedWire, treasuryKey)
	fixturePlan = again
	expired, withLanding = true, true
	done, err = s.RecoverSettlement(ctx, client, again.ID, solana.Signature{})
	if err != nil || done.State != "delivered" {
		t.Fatalf("recover landed draft %v", err)
	}
	if amount, open := paid(other); amount != "15000000" || open {
		t.Fatalf("recovered paid %s open %t", amount, open)
	}
	if tx2.Signatures[0] == landed.Signatures[0] {
		t.Fatal("replacement reused original message")
	}
	if _, err = pool.Exec(ctx, "UPDATE loyal_yield.referral_earn_max_settlements SET quoted_micro_usdc=7 WHERE settlement_id=$1", p1.ID); err == nil {
		t.Fatal("mutated settled terms")
	}
}
