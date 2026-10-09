package fleet

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// A route that opens its target obligation runs init under the vault's setup
// policy, so its typed manifest names both policies. The waiting-ALT request
// must bind the setup policy like the route policy (Rust names it in every
// route manifest); before, every such route was refused as naming an unbound
// policy and could never request its lookup table.
func TestWaitingALTBindsTheVaultSetupPolicy(t *testing.T) {
	s, ctx := waitingALTStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	l, _ := waitingIdentity(t, ctx, s, suffix)
	settingsHash := sha256.Sum256([]byte("waiting-settings:" + suffix))
	setupKey, _, err := squads.PolicyAddress(settingsHash, 2)
	if err != nil {
		t.Fatal(err)
	}
	setup := setupKey.String()
	var setupID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature)
SELECT settings,authority,2,$2,vault_index,vault_pubkey,delegated_signers,threshold,ARRAY[]::text[],stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,true,last_seen_slot,last_seen_signature||':setup'
FROM loyal_yield.route_policies WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1) RETURNING id`, l.VaultID, setup).Scan(&setupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET setup_policy_id=$2 WHERE id=$1`, l.VaultID, setupID); err != nil {
		t.Fatal(err)
	}
	var settings string
	if err := s.pool.QueryRow(ctx, `SELECT settings FROM loyal_yield.managed_vaults WHERE id=$1`, l.VaultID).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	payer := manifestKey(90)
	input := KaminoSameMintRouteRequest{Vault: l.VaultPubkey, Source: KaminoPositionAccounts{Market: manifestKey(4), Reserve: l.SourceReserve, LiquidityMint: USDCMint, Obligation: manifestKey(60), VaultLiquidityATA: manifestKey(61)}}
	input.Target = input.Source
	input.Target.Reserve, input.Target.Obligation = l.TargetReserve, manifestKey(62)
	accounts := []InstructionAccount{{settings, false, false}, {l.VaultPubkey, false, true}, {l.PolicyAccount, false, false}, {setup, false, false}, {input.Source.Market, false, false}, {input.Source.Reserve, false, true}, {input.Target.Reserve, false, true}, {USDCMint, false, false}, {input.Source.Obligation, false, true}, {input.Target.Obligation, false, true}, {input.Source.VaultLiquidityATA, false, true}}
	manifest, err := buildRouteALTManifest(input, settings, []string{l.PolicyAccount, setup}, payer, []RouteInstruction{{Program: kamino.ProgramID.String(), Accounts: accounts, Data: []byte{1}}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, a := range manifest.VaultAddresses {
		named = named || (a.Address == setup && strings.Contains(a.AccountRole, "policy"))
	}
	if !named {
		t.Fatal("fixture manifest does not name the setup policy")
	}
	seedWaitingCatalog(t, ctx, s, l.Cluster, manifest.SharedAddresses)
	p := waitingManifestPreparation(manifest)
	missing := []string{manifest.VaultAddresses[0].Address}
	unbound := l
	if _, err := upsertWaitingFixture(ctx, s, unbound, p, missing); err == nil || !strings.Contains(err.Error(), "unbound policy") {
		t.Fatalf("a policy outside the lease's bound route and setup policies was not refused as unbound: %v", err)
	}
	l.SetupPolicyAccount = setup
	id, err := upsertWaitingFixture(ctx, s, l, p, missing)
	if err != nil {
		t.Fatalf("route needing the vault setup policy cannot request its ALT: %v", err)
	}
	var persisted int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 AND address=$2 AND account_role LIKE '%policy%'`, id, setup).Scan(&persisted); err != nil || persisted != 1 {
		t.Fatalf("setup policy missing from durable demand: count=%d error=%v", persisted, err)
	}
	// The SQL row check still binds the named policy to this vault.
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET active=false WHERE id=$1`, setupID); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWaitingFixture(ctx, s, l, p, missing); err == nil {
		t.Fatal("an inactive setup policy was accepted as bound")
	}
}
