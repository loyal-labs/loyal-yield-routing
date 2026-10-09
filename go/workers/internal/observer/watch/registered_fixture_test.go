package watch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

type watchIdentity struct{ settings, wallet string }
type watchPolicy struct {
	id      int64
	account string
	seed    int64
}
type watchATA struct {
	id               int64
	walletATA, vault string
}
type watchMax struct{ route, vault, policy string }
type registeredFixture struct {
	t        *testing.T
	ctx      context.Context
	yield    *pgxpool.Pool
	prefix   string
	counter  int
	settings []string
}

func registeredWatchFixture(t *testing.T) *registeredFixture {
	t.Helper()
	yieldURL := os.Getenv("TEST_WATCH_DATABASE_URL")
	if yieldURL == "" {
		t.Skip("registered watch Yield fixture URL required")
	}
	parsed, err := url.Parse(yieldURL)
	port := 0
	if parsed != nil {
		port, _ = strconv.Atoi(parsed.Port())
	}
	if err != nil || parsed == nil || parsed.Path != "/workers_v2_observer_watch" || parsed.Hostname() != "127.0.0.1" || port < 1024 || port > 65535 || parsed.User == nil || parsed.User.Username() != "workers_v2" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("registered watch fixture outside dedicated allowlist")
	}
	if _, present := parsed.User.Password(); present {
		t.Fatal("registered watch fixture must not contain credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	yield, err := pgxpool.New(ctx, yieldURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(yield.Close)
	var database, role string
	if err := yield.QueryRow(ctx, `SELECT current_database(),current_user`).Scan(&database, &role); err != nil || database != "workers_v2_observer_watch" || role != "workers_v2" {
		t.Fatalf("registered fixture identity mismatch: %s/%s %v", database, role, err)
	}
	var migrations int
	if err := yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.schema_migrations WHERE version IN(1,36,54,67)`).Scan(&migrations); err != nil || migrations != 4 {
		t.Fatalf("registered Yield migration provenance missing: %v", err)
	}
	// Production Yield has no Apps identity tables; neither does the fixture.
	var appsAbsent bool
	if err := yield.QueryRow(ctx, `SELECT to_regclass('public.app_users') IS NULL AND to_regclass('public.app_user_smart_accounts') IS NULL`).Scan(&appsAbsent); err != nil || !appsAbsent {
		t.Fatal("Yield watch fixture must not contain Apps identity tables")
	}
	f := &registeredFixture{t: t, ctx: ctx, yield: yield, prefix: fmt.Sprintf("watch:%s:%d", t.Name(), time.Now().UnixNano())}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, table := range []string{"multiply_route_states", "earn_max_policy_sets", "user_yield_positions", "earn_deposit_onboarding_attempts", "cross_mint_swap_policies", "balance_sweep_targets", "managed_vaults", "route_policies"} {
			if _, err := yield.Exec(cleanup, `DELETE FROM loyal_yield.`+table+` WHERE settings=ANY($1::text[])`, f.settings); err != nil {
				t.Errorf("cleanup owned %s: %v", table, err)
			}
		}
	})
	return f
}

func (f *registeredFixture) key(label string) string {
	f.counter++
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", f.prefix, label, f.counter)))
	return solana.PublicKey(digest).String()
}
func (f *registeredFixture) identity() watchIdentity {
	i := watchIdentity{f.key("settings"), f.key("wallet")}
	f.settings = append(f.settings, i.settings)
	return i
}
func (f *registeredFixture) vault(i watchIdentity, index uint8) string {
	settings := solana.MustPublicKeyFromBase58(i.settings)
	vault, _, err := squads.SmartAccountAddress(settings, index)
	if err != nil {
		f.t.Fatal(err)
	}
	return vault.String()
}
func (f *registeredFixture) policy(i watchIdentity, index uint8, active bool, cluster string, slot int64) watchPolicy {
	f.counter++
	seed := int64(f.counter)
	settings := solana.MustPublicKeyFromBase58(i.settings)
	account, _, err := squads.PolicyAddress(settings, uint64(seed))
	if err != nil {
		f.t.Fatal(err)
	}
	p := watchPolicy{account: account.String(), seed: seed}
	if err := f.yield.QueryRow(f.ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,kamino_markets,active,last_seen_slot,last_seen_signature,cluster) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,'watch-fixture',$10) RETURNING id`, i.settings, i.wallet, seed, p.account, index, f.vault(i, index), []string{safeMarkets[0].String()}, active, slot, cluster).Scan(&p.id); err != nil {
		f.t.Fatal(err)
	}
	return p
}
func (f *registeredFixture) managed(i watchIdentity, index uint8, active bool, cluster string, slot int64) (watchPolicy, string, int64) {
	p := f.policy(i, index, active, cluster, slot)
	vault := f.vault(i, index)
	var id int64
	if err := f.yield.QueryRow(f.ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,$2,$3,$4,$5) RETURNING id`, i.settings, index, vault, p.id, active).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return p, vault, id
}
func (f *registeredFixture) onboarding(i watchIdentity) {
	p := f.policy(i, 1, true, "mainnet-beta", 120)
	if _, err := f.yield.Exec(f.ctx, `INSERT INTO loyal_yield.earn_deposit_onboarding_attempts(wallet_address,delegated_signer,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,target_reserve,market,liquidity_mint,status,first_seen_at,updated_at) VALUES($1,$1,$2,1,$3,$4,$5,$6,$7,$8,$9,'policy_pending',now(),now())`, i.wallet, i.settings, f.vault(i, 1), p.id, p.account, p.seed, f.key("reserve"), safeMarkets[0].String(), stablecoins[3].Mint.String()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *registeredFixture) position(i watchIdentity, p watchPolicy, vault string) {
	if _, err := f.yield.Exec(f.ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at) VALUES($1,$3,$2,1,$3,$4,$5,$6,$7,$8,$9,$9,1000000,'watch-deposit','watch-deposit',200,'active',now(),now(),$7,$8,$9,1000000,200,now())`, i.wallet, i.settings, vault, p.id, p.account, p.seed, f.key("reserve"), safeMarkets[0].String(), stablecoins[3].Mint.String()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *registeredFixture) cross(i watchIdentity, active bool, cluster string) string {
	account := f.key("cross-policy")
	if _, err := f.yield.Exec(f.ctx, `INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,active,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,1,$5,$3,'classic',50,1000000,$6,$7,false,'create','confirmed',200,'watch-fixture')`, cluster, i.settings, i.wallet, account, f.vault(i, 1), strings.Repeat("a", 64), active); err != nil {
		f.t.Fatal(err)
	}
	return account
}
func (f *registeredFixture) autodeposit(i watchIdentity, desired bool, status, cluster string, authority, delegation *string) watchATA {
	p := f.policy(i, 1, true, cluster, 200)
	vault := f.vault(i, 1)
	walletATA, err := USDCATA(i.wallet)
	if err != nil {
		f.t.Fatal(err)
	}
	vaultATA, err := USDCATA(vault)
	if err != nil {
		f.t.Fatal(err)
	}
	a := watchATA{walletATA: walletATA, vault: vault}
	if err := f.yield.QueryRow(f.ctx, `INSERT INTO loyal_yield.balance_sweep_targets(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,wallet,wallet_usdc_ata,vault_usdc_ata,wallet_token_ata,vault_token_ata,token_mint,threshold,max_amount_per_period,desired_active,chain_status,last_seen_slot,last_seen_signature,cluster,subscription_authority,recurring_delegation) VALUES($1,$2,$3,$4,1,$5,$2,$6,$7,$6,$7,$8,1,1000000,$9,$10,200,'watch-fixture',$11,$12,$13) RETURNING id`, i.settings, i.wallet, p.seed, p.account, vault, walletATA, vaultATA, stablecoins[3].Mint.String(), desired, status, cluster, authority, delegation).Scan(&a.id); err != nil {
		f.t.Fatal(err)
	}
	return a
}
func (f *registeredFixture) earnMax() watchMax {
	i := f.identity()
	p, vault, _ := f.managed(i, 0, true, "mainnet-beta", 400)
	m := watchMax{route: fmt.Sprintf("%s:%d", f.prefix, f.counter), vault: vault, policy: p.account}
	state, _ := json.Marshal(map[string]any{"schemaVersion": 9, "engineVersion": "earn_max_v2", "routeKey": m.route, "generation": 1, "settings": i.settings, "vaultIndex": 0, "vault": vault, "observedSlot": 400})
	if _, err := f.yield.Exec(f.ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,settings,vault_index,vault,state) VALUES($1,$2,0,$3,$4::jsonb)`, m.route, i.settings, vault, state); err != nil {
		f.t.Fatal(err)
	}
	policies, _ := json.Marshal([]map[string]any{{"account": p.account, "seed": p.seed}})
	if _, err := f.yield.Exec(f.ctx, `INSERT INTO loyal_yield.earn_max_policy_sets(settings,vault_index,vault,manifest_version,manifest_sha256,status,policy_accounts,observed_signature,observed_slot,observed_at,policy_seed_base) VALUES($1,0,$2,'earn-max-v2',$3,'ready',$4::jsonb,'watch-fixture',400,now(),320)`, i.settings, vault, strings.Repeat("a", 64), policies); err != nil {
		f.t.Fatal(err)
	}
	return m
}
