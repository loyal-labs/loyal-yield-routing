package fleetexec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLookupVaultDemandUsesAllSealedCohortsAndRejectsStalePlanner(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	ctx := t.Context()
	store, _ := seedLookupSource(t, ctx, pool, f)
	identity := fmt.Sprintf("lookup-demand-%d", time.Now().UnixNano())
	var policy, vault int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$1,0,$1,ARRAY[$1]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY['usdc']::text[],ARRAY[$1]::text[],ARRAY['usdc']::text[],'[]',true,1000,$1) RETURNING id`, identity).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id) VALUES($1,0,$1,$2) RETURNING id`, identity, policy).Scan(&vault); err != nil {
		t.Fatal(err)
	}
	create := func(fingerprint string, addresses []LookupManifestAddress, cancelled bool) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_requests(cluster,vault_id,route_fingerprint,requirements_fingerprint,desired_shared_hash,desired_vault_hash,desired_shared_address_count,desired_vault_address_count) VALUES('localnet',$1,$2,$2,$3,$4,0,$5) RETURNING id`, vault, fingerprint, lookupManifestHash(nil), lookupManifestHash(addresses), len(addresses)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, a := range addresses {
			if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable) VALUES($1,$2,'vault',$3,$4,$5)`, id, a.Address, a.Ordinal, a.Role, a.Writable); err != nil {
				t.Fatal(err)
			}
		}
		status := "requested"
		if cancelled {
			status = "cancelled"
		}
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at=clock_timestamp(),request_status=$2 WHERE id=$1`, id, status); err != nil {
			t.Fatal(err)
		}
		return id
	}
	create(identity+"-first", []LookupManifestAddress{{Address: f.Addresses[0], SemanticClass: "vault", Role: "r1"}}, false)
	create(identity+"-second", []LookupManifestAddress{{Address: f.Addresses[0], SemanticClass: "vault", Role: "r2,r1", Writable: true}, {Address: f.Addresses[1], SemanticClass: "vault", Ordinal: 1, Role: "r3"}}, false)
	create(identity+"-cancelled", []LookupManifestAddress{{Address: f.Addresses[2], SemanticClass: "vault", Role: "cancelled"}}, true)
	request, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "lookup-planner", time.Minute)
	if err != nil || request == nil {
		t.Fatal("actual source request lease", request, err)
	}
	manifest, err := store.SealLookupVaultDemand(ctx, *request, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Addresses) != 2 || manifest.Addresses[0].Role != "r1,r2" || !manifest.Addresses[0].Writable || manifest.Addresses[1].Role != "r3" || manifest.Addresses[1].Writable {
		t.Fatal("partial cohort replaced full vault intent", manifest)
	}
	// The aggregate is sorted by account address, matching the source BTreeMap.
	if manifest.Addresses[0].Address > manifest.Addresses[1].Address {
		t.Fatal("aggregate order is unstable")
	}
	replay, err := store.SealLookupVaultDemand(ctx, *request, 1001)
	if err != nil || replay.ID != manifest.ID || replay.Hash != manifest.Hash {
		t.Fatal("idempotent manifest changed", replay, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET lease_owner='replacement-planner',fencing_token=fencing_token+1 WHERE id=$1`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SealLookupVaultDemand(context.Background(), *request, 1001); !errors.Is(err, ErrStaleOwner) {
		t.Fatal("stale planner published aggregate", err)
	}
}
