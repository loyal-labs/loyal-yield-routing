package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type staticMarketEpochSource struct{ epoch ImmutableMarketEpoch }

func (s staticMarketEpochSource) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return s.epoch, nil
}

type failedMarketEpochSource struct{ err error }

func (s failedMarketEpochSource) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return ImmutableMarketEpoch{}, s.err
}

func TestWorkerIntegrationCutoverWithoutRustMonitorOrPlanner(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	market := testIdentity(41)
	source := ReserveIdentity{Address: testIdentity(4), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(81), Market: market, Mint: USDCMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, market, source.Address)
	sourceAccount := reserveFixture(source, 1_000_000_000_000_000, 1_000_000_000_000_000)
	setCurveScale(sourceAccount.Data, 30)
	targetAccount := reserveFixture(target, 1_000_000_000_000_000, 1_000_000_000_000_000)
	setCurveScale(targetAccount.Data, 220)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Error(err)
			writer.WriteHeader(400)
			return
		}
		writer.Header().Set("content-type", "application/json")
		switch call.Method {
		case "getSlot":
			json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": 1_000})
		case "getMultipleAccounts":
			accountValue := func(account Account) map[string]any {
				return map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
			}
			var requested []string
			if len(call.Params) == 0 || json.Unmarshal(call.Params[0], &requested) != nil {
				t.Error("missing account request")
				writer.WriteHeader(400)
				return
			}
			values := make([]any, 0, len(requested))
			for _, address := range requested {
				if address == source.Address {
					values = append(values, accountValue(sourceAccount))
				} else if address == target.Address {
					values = append(values, accountValue(targetAccount))
				} else {
					t.Errorf("unknown account %s", address)
					writer.WriteHeader(400)
					return
				}
			}
			json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": 1_000}, "value": values}})
		default:
			t.Errorf("unexpected RPC method %s", call.Method)
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	config := Config{DatabaseURL: databaseURL, TimescaleURL: databaseURL, TimescaleSchema: "kamino", RPCURL: server.URL, Cluster: "localnet", Mode: ModePublish, VaultID: vaultID, Source: source, Target: target, PollInterval: time.Second, SlotDuration: 400 * time.Millisecond}
	worker, err := NewWorker(config, store, NewRPCClient(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	sourceState, err := DecodeKaminoSourceReserve(sourceAccount, source, 1_000, config.SlotDuration)
	if err != nil {
		t.Fatal(err)
	}
	targetState, err := DecodeKaminoReserve(targetAccount, target, 1_000, config.SlotDuration)
	if err != nil {
		t.Fatal(err)
	}
	evidenceSnapshot := MarketSnapshot{Slot: 1_000, ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{source.Address: sourceState, target.Address: targetState}}
	epoch := testImmutableMarketEpoch(t, evidenceSnapshot, source, target)
	// A blocked mint remains part of the catalog/epoch identity but contributes
	// no routable reserves. Healthy USDC planning must continue independently.
	epoch.CatalogReserveCount++
	epoch.MintCoverage = append(epoch.MintCoverage, MarketMintCoverage{
		Mint: USDTMint, CatalogReserveCount: 1, Blockers: []MarketMintBlocker{{Code: "missing_verified_reserve", Detail: "test blocked mint"}},
	})
	epoch.Fingerprint = epochFingerprint(epoch.Reserves, sortedUnique(coverageMints(epoch.MintCoverage)), epoch.CatalogFingerprint, epoch.MintCoverage)
	epoch.OptimizerEpochID = positiveEpochID(epoch.Fingerprint)
	if err := epoch.Validate(); err != nil {
		t.Fatalf("healthy plus blocked mint epoch was invalid: %v", err)
	}
	if err := worker.SetMarketEvidence(staticMarketEpochSource{epoch: epoch}); err != nil {
		t.Fatal(err)
	}
	expectedSnapshot, err := marketSnapshotFromEpoch(epoch, source.Address, target.Address)
	if err != nil {
		t.Fatal(err)
	}
	expectedSnapshot.Cluster = config.Cluster
	expectedSnapshot.OptimizerEpochID, err = store.EnsureOptimizerEpoch(ctx, config.Cluster, epoch)
	if err != nil {
		t.Fatal(err)
	}
	expectedFleet, err := store.LoadMigratedFleet(ctx, config.Cluster, epoch)
	if err != nil {
		t.Fatal(err)
	}
	expectedPlan, err := PlanFleet(expectedSnapshot, expectedFleet)
	if err != nil || len(expectedPlan.Opportunities) != 1 {
		t.Fatalf("expected fleet plan: %#v %v", expectedPlan, err)
	}
	if err := worker.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	var state, fingerprint, opportunityKey string
	var slot int64
	err = store.pool.QueryRow(ctx, `SELECT count(*)::bigint,min(opportunity.opportunity_state),min(epoch.market_state->>'fingerprint'),min(epoch.market_slot),min(opportunity.idempotency_key) FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id WHERE opportunity.vault_id=$1`, vaultID).Scan(&count, &state, &fingerprint, &slot, &opportunityKey)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || state != "revalidate" || fingerprint != epoch.Fingerprint || slot != 1_000 || opportunityKey != expectedPlan.Opportunities[0].IdempotencyKey {
		t.Fatalf("confirmed update did not reach durable W3 queue: count=%d state=%s fingerprint=%s slot=%d key=%s expected=%s", count, state, fingerprint, slot, opportunityKey, expectedPlan.Opportunities[0].IdempotencyKey)
	}
	var registeredAt, lastSeenAt time.Time
	if err := store.pool.QueryRow(ctx, `SELECT registered_at,last_seen_at FROM loyal_yield.fleet_planning_clusters WHERE cluster=$1`, config.Cluster).Scan(&registeredAt, &lastSeenAt); err != nil || registeredAt.IsZero() || lastSeenAt.IsZero() {
		t.Fatalf("initial planner registration missing: registered=%s seen=%s err=%v", registeredAt, lastSeenAt, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.fleet_planning_clusters SET last_seen_at=clock_timestamp()-interval '1 hour' WHERE cluster=$1`, config.Cluster); err != nil {
		t.Fatal(err)
	}
	if err := worker.SetMarketEvidence(failedMarketEpochSource{err: fmt.Errorf("persistent Timescale failure")}); err != nil {
		t.Fatal(err)
	}
	if err := worker.planningCycle(ctx); err == nil {
		t.Fatal("failed planning pass unexpectedly succeeded")
	}
	var heartbeatFresh bool
	if err := store.pool.QueryRow(ctx, `SELECT last_seen_at>clock_timestamp()-interval '1 minute' FROM loyal_yield.fleet_planning_clusters WHERE cluster=$1`, config.Cluster).Scan(&heartbeatFresh); err != nil || heartbeatFresh {
		t.Fatalf("failed planning pass refreshed successful-cycle health: fresh=%t err=%v", heartbeatFresh, err)
	}
	if err := worker.SetMarketEvidence(staticMarketEpochSource{epoch: epoch}); err != nil {
		t.Fatal(err)
	}
	if err := worker.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT last_seen_at>clock_timestamp()-interval '1 minute' FROM loyal_yield.fleet_planning_clusters WHERE cluster=$1`, config.Cluster).Scan(&heartbeatFresh); err != nil || !heartbeatFresh {
		t.Fatalf("planner heartbeat was not refreshed: fresh=%t err=%v", heartbeatFresh, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE vault_id=$1`, vaultID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("same confirmed evidence produced %d opportunities", count)
	}
}

func seedWorkerVault(t *testing.T, ctx context.Context, store *Store, suffix, market, source string) int64 {
	t.Helper()
	var policyID, vaultID, snapshotID int64
	err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$5]::text[],ARRAY[$6]::text[],ARRAY[$5]::text[],'[]',true,999,$7) RETURNING id`, `settings:`+suffix, `authority:`+suffix, `policy:`+suffix, `vault:`+suffix, USDCMint, market, `signature:`+suffix).Scan(&policyID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, `settings:`+suffix, `vault:`+suffix, policyID).Scan(&vaultID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,is_current,context) VALUES($1,$2,999,clock_timestamp(),true,'{}') RETURNING id`, vaultID, policyID).Scan(&snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) VALUES($1,$2,$3,$4,900000000000,true,100,$5,999,clock_timestamp(),'{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"1000000000000"}')`, vaultID, source, market, USDCMint, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	return vaultID
}

func setCurveScale(data []byte, scale uint32) {
	config := data[reserveConfigOffset:]
	for index := 0; index < 11; index++ {
		offset := 64 + index*8
		binary.LittleEndian.PutUint32(config[offset+4:offset+8], uint32(index)*scale)
	}
}
