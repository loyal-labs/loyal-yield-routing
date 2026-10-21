package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type staticCatalog []ReserveIdentity

func (c staticCatalog) LoadCatalog(context.Context) ([]ReserveIdentity, error) {
	return append([]ReserveIdentity(nil), c...), nil
}

// uniqueKey is a valid public key unique to this run.
func uniqueKey(label string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", label, time.Now().UnixNano())))
	return solana.PublicKeyFromBytes(sum[:]).String()
}

// chainFixture serves accounts over getMultipleAccounts at one slot and
// records the slot floor of every read.
type chainFixture struct {
	mu       sync.Mutex
	slot     int
	accounts map[string]chain.Account
	floors   []uint64
}

func (f *chainFixture) set(account chain.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[account.Key.String()] = account
}

func (f *chainFixture) delete(address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accounts, address)
}

func (f *chainFixture) serve(t *testing.T) *chain.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			w.WriteHeader(400)
			return
		}
		var result any
		switch call.Method {
		case "getRecentPerformanceSamples":
			result = testPerformanceSamples
		case "getMultipleAccounts":
			var requested []string
			var opts struct {
				MinContextSlot uint64 `json:"minContextSlot"`
			}
			_ = json.Unmarshal(call.Params[0], &requested)
			_ = json.Unmarshal(call.Params[len(call.Params)-1], &opts)
			f.mu.Lock()
			f.floors = append(f.floors, opts.MinContextSlot)
			values := make([]any, len(requested))
			for i, address := range requested {
				if account, ok := f.accounts[address]; ok {
					values[i] = map[string]any{"owner": account.Owner.String(), "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
				}
			}
			slot := f.slot
			f.mu.Unlock()
			result = map[string]any{"context": map[string]any{"slot": slot}, "value": values}
		default:
			t.Errorf("unexpected RPC method %s", call.Method)
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	return testChain(t, server.URL)
}

// obligationAccount is the vault's vanilla obligation in market holding
// collateral in each reserve.
func obligationAccount(t *testing.T, vault, market string, collateral map[string]uint64) chain.Account {
	t.Helper()
	address, err := kamino.VanillaObligation(solana.MustPublicKeyFromBase58(vault), solana.MustPublicKeyFromBase58(market))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, kamino.ObligationSize)
	copy(data, kamino.ObligationDiscriminator[:])
	fixtureKey(t, data, 32, market)
	fixtureKey(t, data, 64, vault)
	slot := 0
	for reserve, amount := range collateral {
		fixtureKey(t, data, 96+slot*136, reserve)
		binary.LittleEndian.PutUint64(data[96+slot*136+32:], amount)
		slot++
	}
	return fixtureAccount(address.String(), kamino.ProgramID.String(), 23_942_400, data)
}

// One tick reads the catalog reserves and the registered vault's policy and
// obligation over RPC, plans its holding from that read, and makes the
// queue the plan: a fresh move is published, kept while it is unclaimed and
// the same, left alone while claimed and superseded once the plan no longer
// holds it.
func TestPlannerTickPlansFromOneChainSnapshot(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "planner-" + suffix
	market := uniqueKey("market")
	source := ReserveIdentity{Address: uniqueKey("source"), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: uniqueKey("target"), Market: market, Mint: USDCMint}
	signer, settings, vault, policy := uniqueKey("signer"), uniqueKey("settings"), uniqueKey("vault"), uniqueKey("policy")
	var policyID, vaultID int64
	if err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,1,$4,0,$5,ARRAY[$3]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$6]::text[],ARRAY[$7]::text[],ARRAY[$6]::text[],'[]',true,'finalized',true,999,$8) RETURNING id`, cluster, settings, signer, policy, vault, USDCMint, market, "signature:"+suffix).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, settings, vault, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	reserve := func(identity ReserveIdentity, scale uint32) chain.Account {
		account := reserveFixture(identity, 1_000_000_000_000_000, 1_000_000_000_000_000)
		setCurveScale(account.Data, scale)
		// One collateral unit redeems one liquidity unit.
		binary.LittleEndian.PutUint64(account.Data[2592:2600], 2_000_000_000_000_000)
		return account
	}
	policyData, err := buildExactPolicyFixture(settings, signer, 0, []RouteInstruction{{Program: kamino.ProgramID.String(), Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &chainFixture{slot: 1_000, accounts: map[string]chain.Account{}}
	fixture.set(reserve(source, 30))
	fixture.set(reserve(target, 220))
	fixture.set(fixtureAccount(policy, squads.ProgramID.String(), 1, policyData))
	fixture.set(obligationAccount(t, vault, market, map[string]uint64{source.Address: 1_000_000_000_000}))
	config := Config{DatabaseURL: databaseURL, TimescaleURL: databaseURL, TimescaleSchema: "kamino", RPCURL: "http://localhost", Cluster: cluster, PollInterval: time.Second, DelegatedSigner: signer}
	registry := prometheus.NewRegistry()
	worker, err := NewWorker(config, store, fixture.serve(t), staticCatalog{source, target}, engine.NewFacts(registry))
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id              int64
		state, from, to string
		amount, epoch   int64
	}
	queue := func() []row {
		t.Helper()
		rows, err := store.pool.Query(ctx, `SELECT id,opportunity_state,source_reserve,target_reserve,amount_raw,optimizer_epoch_id FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 ORDER BY id`, cluster)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.state, &r.from, &r.to, &r.amount, &r.epoch); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	if err := worker.planningCycle(ctx); err != nil {
		t.Fatal(err)
	}
	moves := queue()
	if len(moves) != 1 || moves[0].state != "revalidate" || moves[0].from != source.Address || moves[0].to != target.Address || moves[0].amount != 1_000_000_000_000 {
		t.Fatalf("tick did not plan the chain holding: %+v\n%s", moves, logs.String())
	}
	// Every vault read is served at or after the catalog reserves' slot.
	fixture.mu.Lock()
	floors := append([]uint64(nil), fixture.floors...)
	fixture.mu.Unlock()
	if len(floors) != 2 || floors[0] != 0 || floors[1] != 1_000 {
		t.Fatalf("snapshot reads were not pinned to the reserves' slot: %v", floors)
	}
	// The tick's reserves are recorded once, as history the move refers to.
	var marketSlot int64
	var reserves int
	var observedAt *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT market_slot,jsonb_array_length(market_state->'reserves'),(SELECT (r->>'observedAt')::timestamptz FROM jsonb_array_elements(market_state->'reserves') r WHERE r->>'reserve'=$2) FROM loyal_yield.optimizer_epochs WHERE id=$1`, moves[0].epoch, target.Address).Scan(&marketSlot, &reserves, &observedAt); err != nil {
		t.Fatal(err)
	}
	if marketSlot != 1_000 || reserves != 2 || observedAt == nil {
		t.Fatalf("snapshot record slot=%d reserves=%d target observed=%v", marketSlot, reserves, observedAt)
	}
	var seen *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT last_seen_at FROM loyal_yield.fleet_planning_clusters WHERE cluster=$1`, cluster).Scan(&seen); err != nil || seen == nil {
		t.Fatalf("a complete tick did not heartbeat: %v %v", seen, err)
	}
	// The next tick plans the same move: the unclaimed row is kept.
	if err := worker.planningCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if again := queue(); len(again) != 1 || again[0].id != moves[0].id || again[0].state != "revalidate" {
		t.Fatalf("an unchanged unclaimed move was not kept: %+v", again)
	}
	// A claimed move is the executor's: the vault leaves the registry, the
	// row is untouched and its principal is in flight.
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='leased',lease_kind='revalidate',lease_owner='executor',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, moves[0].id); err != nil {
		t.Fatal(err)
	}
	if err := worker.planningCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if claimed := queue(); len(claimed) != 1 || claimed[0].state != "leased" {
		t.Fatalf("the plan touched claimed work: %+v", claimed)
	}
	inflow, outflow, err := InflightFlows(ctx, store.pool, cluster, 1_000, 0)
	if err != nil || inflow[target.Address] != 1_000_000_000_000 || outflow[source.Address] != 1_000_000_000_000 {
		t.Fatalf("claimed move not in flight: in=%v out=%v err=%v", inflow, outflow, err)
	}
	// The executor's lease ends without a decision and the chain shows the
	// holding already in the better reserve: the plan holds no move, so the
	// abandoned row is superseded.
	fixture.set(obligationAccount(t, vault, market, map[string]uint64{target.Address: 1_000_000_000_000}))
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET created_at=created_at-interval '1 minute',lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, moves[0].id); err != nil {
		t.Fatal(err)
	}
	worker.runtimeCycle(ctx)
	if after := queue(); len(after) != 1 || after[0].state != "superseded" {
		t.Fatalf("the abandoned move outlived the plan: %+v", after)
	}
	if at := plannerLaneSucceededAt(t, registry); at == 0 {
		t.Fatal("complete ticks did not record planner lane success")
	}
}
