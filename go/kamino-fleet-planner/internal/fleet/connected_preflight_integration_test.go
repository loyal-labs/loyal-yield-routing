package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"
)

func fixtureKey(t *testing.T, data []byte, offset int, address string) {
	t.Helper()
	key, err := decodePublicKey(address)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[offset:offset+32], key[:])
}

func connectedPolicyHeader(t *testing.T, settings, signer string, seed uint64) ([]byte, string) {
	t.Helper()
	data, err := BuildExactPolicyFixture(settings, signer, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	address, bump, err := derivePolicyAccount(settings, seed)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[40:48], seed)
	data[48] = bump
	return data[:110], address
}

func connectedSwapPolicy(t *testing.T, binding CrossMintPolicyBindings, seed uint64) []byte {
	t.Helper()
	data, _ := connectedPolicyHeader(t, binding.Settings, binding.DelegatedSigner, seed)
	data = appendU32x(data, 2)
	for i, disc := range [][]byte{jupiterRouteV2Discriminator, jupiterSharedV2Discriminator} {
		key, _ := decodePublicKey(jupiterProgram)
		data = append(data, key[:]...)
		data = appendU32x(data, 2)
		indexes := []byte{0, 2}
		offset := uint64(24)
		if i == 1 {
			indexes = []byte{1, 5}
			offset = 25
		}
		for j, index := range indexes {
			keys := []string{binding.VaultPubkey}
			if j == 1 {
				keys = nil
				for _, mint := range earnStableMints {
					ata, err := deriveATA(binding.VaultPubkey, mint, mustStableProgram(mint))
					if err != nil {
						t.Fatal(err)
					}
					keys = append(keys, ata)
				}
			}
			data = append(data, index, 0)
			data = appendU32x(data, uint32(len(keys)))
			for _, k := range keys {
				key, _ := decodePublicKey(k)
				data = append(data, key[:]...)
			}
			data = append(data, 0)
		}
		data = appendU32x(data, 3)
		data = appendU64x(data, 0)
		data = append(data, 5)
		data = appendU32x(data, uint32(len(disc)))
		data = append(data, disc...)
		data = append(data, 0)
		data = appendU64x(data, offset)
		data = append(data, 1)
		data = appendU16x(data, binding.Swap.MaxSlippageBPS)
		data = append(data, 5)
		data = appendU64x(data, offset+2)
		data = append(data, 0, 0, 0)
	}
	data = append(data, 0, 0)
	data = appendU32x(data, 3)
	for _, mint := range []string{USDCMint, USDTMint, USDSMint} {
		key, _ := decodePublicKey(mint)
		data = append(data, key[:]...)
		data = appendU64x(data, 0)
		data = append(data, 0, 1, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
		data = append(data, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
	}
	data = appendU64x(data, 0)
	data = append(data, 0)
	data = append(data, make([]byte, 32)...)
	return data
}

// This test runs the real Go planner, claim, RPC/Jupiter validation, proxy and
// durable commit. RPC transport is local; exact transaction simulation executes
// through Squads and mock protocol SBF in LiteSVM, not a canned success response.
// This preflight alone does not prove the retained Rust submission lifecycle.
// Constrain actual instruction accounts as well as data. Both market keys are
// authorized by the same Earn policy, as required by retained policy discovery.
func connectedEarnPolicyData(settings, signer string, instructions []RouteInstruction, positions []KaminoPositionAccounts) ([]byte, error) {
	b, err := BuildExactPolicyFixture(settings, signer, 0, nil)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(b[len(b)-4:], uint32(len(instructions)))
	for _, ix := range instructions {
		program, err := decodePublicKey(ix.Program)
		if err != nil {
			return nil, err
		}
		b = append(b, program[:]...)
		b = appendU32x(b, uint32(len(ix.Accounts)))
		for i, account := range ix.Accounts {
			keys := []string{account.Address}
			for _, p := range positions {
				if account.Address == p.Market {
					keys = []string{positions[0].Market, positions[1].Market}
				}
			}
			b = append(b, byte(i), 0)
			b = appendU32x(b, uint32(len(keys)))
			for _, key := range keys {
				decoded, err := decodePublicKey(key)
				if err != nil {
					return nil, err
				}
				b = append(b, decoded[:]...)
			}
			b = append(b, 0)
		}
		b = appendU32x(b, 1)
		b = appendU64x(b, 0)
		b = append(b, 5)
		b = appendU32x(b, uint32(len(ix.Data)))
		b = append(b, ix.Data...)
		b = append(b, 0)
	}
	return b, nil
}

func TestConnectedCrossMintPreflight(t *testing.T) { runConnectedLane(t, false) }
func TestConnectedSameMintLifecycle(t *testing.T)  { runConnectedLane(t, true) }

func runConnectedLane(t *testing.T, sameMint bool) {
	databaseURL, proxyPath := os.Getenv("FLEET_TEST_DATABASE_URL"), os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	if sameMint {
		databaseURL = os.Getenv("FLEET_TEST_SAME_MINT_DATABASE_URL")
	}
	if databaseURL == "" || proxyPath == "" {
		t.Skip("requires disposable database and real KLend proxy")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || u.Hostname() != "127.0.0.1" || (u.Path != "/fleet" && u.Path != "/fleet_same_mint") {
		t.Fatal("requires disposable loopback /fleet database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewKLendProxy(proxyPath, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	signer := encodeBase58(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey))
	settings := testIdentity(12)
	settingsKey := solana.MustPublicKeyFromBase58(settings)
	vaultKey, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), settingsKey[:], []byte("smart_account"), {0}}, solana.MustPublicKeyFromBase58(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	vault := vaultKey.String()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "localnet"
	source := ReserveIdentity{testIdentity(51), testIdentity(52), USDCMint}
	target := ReserveIdentity{testIdentity(61), testIdentity(62), USDTMint}
	if sameMint {
		target.Mint = USDCMint
	}
	const amount = uint64(1_000_000_000)
	accounts := map[string]Account{}
	positions := []KaminoPositionAccounts{}
	states := map[string]ReserveState{}
	for i, identity := range []ReserveIdentity{source, target} {
		account := reserveFixture(identity, 1_000_000_000_000, 1_000_000_000_000)
		setCurveScale(account.Data, uint32(50+i*350))
		fixtureKey(t, account.Data, 408, tokenProgram)
		fixtureKey(t, account.Data, 2560, testIdentity(byte(90+i)))
		fixtureKey(t, account.Data, 160, testIdentity(byte(92+i)))
		fixtureKey(t, account.Data, 2600, testIdentity(byte(94+i)))
		binary.LittleEndian.PutUint64(account.Data[2592:2600], 2_000_000_000_000)
		accounts[account.Address] = account
		decoded, err := decodeRouteReserve(account, vault)
		if err != nil {
			t.Fatal(err)
		}
		obligation := Account{Address: decoded.Obligation, Owner: KLendProgram, Lamports: 1_000_000, Data: make([]byte, 3344)}
		copy(obligation.Data, []byte{168, 206, 141, 106, 88, 76, 172, 167})
		fixtureKey(t, obligation.Data, 32, identity.Market)
		fixtureKey(t, obligation.Data, 64, vault)
		if i == 0 {
			fixtureKey(t, obligation.Data, 96, identity.Address)
			binary.LittleEndian.PutUint64(obligation.Data[128:136], amount)
		}
		if _, err = decodeObligation(obligation, identity.Market, vault, map[bool]string{true: identity.Address, false: ""}[i == 0], &decoded.Position); err != nil {
			t.Fatal(err)
		}
		accounts[obligation.Address] = obligation
		positions = append(positions, decoded.Position)
		state, err := DecodeKaminoReserve(account, identity, 1000, 400*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		states[identity.Address] = state
		mint := Account{Address: identity.Mint, Owner: tokenProgram, Lamports: 1_000_000, Data: make([]byte, 82)}
		mint.Data[44] = 6
		mint.Data[45] = 1
		binary.LittleEndian.PutUint64(mint.Data[36:44], 2_000_000_000_000)
		accounts[mint.Address] = mint
		ata := Account{Address: decoded.Position.VaultLiquidityATA, Owner: tokenProgram, Lamports: 1_000_000, Data: make([]byte, 165)}
		fixtureKey(t, ata.Data, 0, identity.Mint)
		fixtureKey(t, ata.Data, 32, vault)
		ata.Data[108] = 1
		accounts[ata.Address] = ata
	}
	body, _ := jupiterBuildForVault(t, vault, amount-1, amount-1, 1)
	var envelope rawJupiterBuild
	if err = json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	minimum := thresholdFor(amount-1, 1)
	var route KaminoSameMintRoute
	if sameMint {
		route, err = proxy.Build(ctx, KaminoSameMintRouteRequest{vault, positions[0], positions[1], amount, amount})
	} else {
		route, err = proxy.BuildCrossMintLegs(ctx, KaminoSameMintRouteRequest{vault, positions[0], positions[1], amount - 1, minimum})
	}
	if err != nil {
		t.Fatal(err)
	}
	policyInstructions := append([]RouteInstruction(nil), route.Protected...)
	// Deposit amount is finalized swap custody, not the preflight minimum.
	// Authorize the deposit discriminator; retained custody checks bound amount.
	policyInstructions[1].Data = policyInstructions[1].Data[:8]
	policyData, err := connectedEarnPolicyData(settings, signer, policyInstructions, positions)
	if err != nil {
		t.Fatal(err)
	}
	_, earnPolicy := connectedPolicyHeader(t, settings, signer, 1)
	_, earnBump, _ := derivePolicyAccount(settings, 1)
	binary.LittleEndian.PutUint64(policyData[40:48], 1)
	policyData[48] = earnBump
	// BuildExactPolicyFixture emits the constraint prefix used by decoder tests.
	// An executable Squads account also needs hooks, spending limits, start,
	// expiration and rent collector (the full account tail).
	policyData = append(policyData, make([]byte, 2+4+8+1+32)...)
	accounts[earnPolicy] = Account{Address: earnPolicy, Owner: SquadsProgram, Lamports: 1_000_000, Data: policyData}
	_, swapPolicy := connectedPolicyHeader(t, settings, signer, 11)
	binding := CrossMintPolicyBindings{Settings: settings, VaultPubkey: vault, DelegatedSigner: signer, Withdraw: CrossMintEarnPolicyBinding{earnPolicy, 999, "local-withdraw", "finalized", 0}, Deposit: CrossMintEarnPolicyBinding{earnPolicy, 999, "local-deposit", "finalized", 1}, Swap: CrossMintSwapPolicyBinding{PolicyAccount: swapPolicy, SourceShard: "classic", EnrollmentGeneration: 1, ObservedSlot: 999, ObservedSignature: "local-swap", SourceCommitment: "finalized", MaxSlippageBPS: 50, DailySourceMintSpendingCap: 10_000_000_000}}
	binding.Swap.ManifestFingerprint = fingerprintCrossMintManifest(binding, []string{USDCMint, USDTMint, USDSMint}, tokenProgram)
	swapData := connectedSwapPolicy(t, binding, 11)
	accounts[swapPolicy] = Account{Address: swapPolicy, Owner: SquadsProgram, Lamports: 1_000_000, Data: swapData}
	withdraw, err := wrapSquadsPolicy(earnPolicy, signer, 0, []uint8{0}, []RouteInstruction{route.Protected[0]})
	if err != nil {
		t.Fatal(err)
	}
	withdrawCovered := requiredLookupTableAddresses(append(append([]RouteInstruction{}, route.Public...), withdraw))
	coverage := map[string]bool{}
	for _, key := range withdrawCovered {
		coverage[key] = true
	}
	coverage[envelope.SwapInstruction.ProgramID] = true
	for _, account := range envelope.SwapInstruction.Accounts {
		if !account.IsSigner {
			coverage[account.Pubkey] = true
		}
	}
	covered := make([]string, 0, len(coverage))
	for key := range coverage {
		covered = append(covered, key)
	}
	sort.Strings(covered)
	tableAddress := testIdentity(245)
	table := Account{Address: tableAddress, Owner: altProgram, Lamports: 1_000_000, Data: make([]byte, 56+32*len(covered))}
	binary.LittleEndian.PutUint32(table.Data[:4], 1)
	binary.LittleEndian.PutUint64(table.Data[4:12], ^uint64(0))
	binary.LittleEndian.PutUint64(table.Data[12:20], 900)
	table.Data[21] = 1
	fixtureKey(t, table.Data, 22, signer)
	for i, key := range covered {
		fixtureKey(t, table.Data, 56+32*i, key)
	}
	accounts[tableAddress] = table
	sharedSet := map[string]bool{farmsProgram: true, instructionsSysvar: true, tokenProgram: true}
	for _, p := range positions {
		for _, key := range []string{p.Reserve, p.Market, p.MarketAuthority, p.LiquidityMint, p.CollateralMint, p.LiquiditySupply, p.CollateralSupply} {
			sharedSet[key] = true
		}
	}
	sharedAddresses := make([]string, 0, len(sharedSet))
	for key := range sharedSet {
		sharedAddresses = append(sharedAddresses, key)
	}
	sort.Strings(sharedAddresses)
	vaultSet := map[string]bool{swapPolicy: true}
	for _, key := range withdrawCovered {
		if !sharedSet[key] {
			vaultSet[key] = true
		}
	}
	for _, p := range positions {
		vaultSet[p.Obligation] = true
		vaultSet[p.VaultLiquidityATA] = true
	}
	vaultAddresses := make([]string, 0, len(vaultSet))
	for key := range vaultSet {
		vaultAddresses = append(vaultAddresses, key)
	}
	sort.Strings(vaultAddresses)
	sharedTable, vaultTable := testIdentity(246), testIdentity(247)
	for key, addresses := range map[string][]string{sharedTable: sharedAddresses, vaultTable: vaultAddresses} {
		data := make([]byte, 56+32*len(addresses))
		copy(data, table.Data[:56])
		for i, address := range addresses {
			fixtureKey(t, data, 56+32*i, address)
		}
		accounts[key] = Account{Address: key, Owner: altProgram, Lamports: 100_000_000, Data: data}
	}
	envelope.AddressesByLookupTableAddress = map[string][]string{tableAddress: covered}
	body, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	seedConnectedExecutionAccounts(t, accounts, positions, signer, vault)
	svm := startConnectedSVM(t, ctx, accounts)
	blockhash, err := decodePublicKey(svm.blockhash)
	if err != nil {
		t.Fatal(err)
	}
	envelope.BlockhashWithMetadata.Blockhash = blockhash[:]
	envelope.BlockhashWithMetadata.LastValidBlockHeight = 1150
	body, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	// Jupiter's real wire format is a numeric byte array, not Go's default
	// base64 encoding for []byte. Both language consumers must see that wire.
	var wire map[string]any
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	blockhashNumbers := make([]int, len(blockhash))
	for i, b := range blockhash {
		blockhashNumbers[i] = int(b)
	}
	wire["blockhashWithMetadata"].(map[string]any)["blockhash"] = blockhashNumbers
	body, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var simulated atomic.Int64
	var ambiguousBroadcasts atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/build" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		var request struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if request.Method == "simulateTransaction" {
			simulated.Add(1)
		}
		// All chain reads and writes share the same SVM state. In particular,
		// never serve seeded balances after a transaction has changed them.
		response, err := svm.call(request)
		if err != nil {
			t.Errorf("SVM transport: %v", err)
			http.Error(w, "SVM transport failed", 500)
			return
		}
		if request.Method == "sendTransaction" && ambiguousBroadcasts.CompareAndSwap(0, 1) {
			// The chain executed the persisted wire, but the broadcaster lost
			// its response. Retained status polling must recover without signing.
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32000, "message": "injected response loss after local execution"}})
			return
		}
		if request.Method == "simulateTransaction" {
			t.Logf("local execution simulation: %s", response)
		}
		_, _ = w.Write(response)
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	workerRPC := httptest.NewServer(handler)
	defer workerRPC.Close()
	vaultID := seedWorkerVault(t, ctx, store, suffix, source.Market, source.Address)
	statements := []struct {
		sql  string
		args []any
	}{
		{`UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,policy_account=$4,vault_pubkey=$5,delegated_signers=ARRAY[$3]::text[],cluster=$6,source_commitment='finalized',finalized_eligible=true,stable_mints=ARRAY[$7,$8]::text[],kamino_markets=ARRAY[$9,$10]::text[],kamino_liquidity_mints=ARRAY[$7,$8]::text[] WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, []any{vaultID, settings, signer, earnPolicy, vault, cluster, USDCMint, USDTMint, source.Market, target.Market}},
		{`UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, []any{vaultID, settings, vault}},
		{`UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=$2,planning_metadata=jsonb_build_object('amount_semantics','kamino_obligation_collateral_deposited_amount','redeemable_source_liquidity_amount_raw',$3::text) WHERE vault_id=$1`, []any{vaultID, int64(amount), strconv.FormatUint(amount, 10)}},
	}
	for _, s := range statements {
		if _, err = store.pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	if sameMint {
		if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET planning_metadata=planning_metadata || '{"idle_vault_liquidity_amount_raw":"0"}'::jsonb WHERE vault_id=$1`, vaultID); err != nil {
			t.Fatal(err)
		}
		// The finalized initial snapshot includes the empty target obligation
		// observed in the local chain, not merely the funded source position.
		_, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) SELECT vault_id,$2,$3,$4,0,false,$5,snapshot_id,observed_slot,observed_at,'{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"0"}'::jsonb FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$6`, vaultID, target.Address, target.Market, target.Mint, states[target.Address].SupplyAPYBPS, source.Address)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Seed enrollment/catalog inputs only; queue work still comes exclusively
	// from Go publication. The sibling shard is not an execution lane here.
	_, siblingPolicy := connectedPolicyHeader(t, settings, signer, 12)
	if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_vault_opt_ins(cluster,settings,vault_index,vault_pubkey,enabled,classic_policy_account,classic_policy_seed,token_2022_policy_account,token_2022_policy_seed,max_slippage_bps,daily_source_mint_spending_cap,generation) VALUES($1,$2,0,$3,true,$4,11,$5,12,$6,$7,1)`, cluster, settings, vault, swapPolicy, siblingPolicy, binding.Swap.MaxSlippageBPS, int64(binding.Swap.DailySourceMintSpendingCap)); err != nil {
		t.Fatal(err)
	}
	for _, shard := range []struct {
		name, account string
		seed          int64
	}{{"classic", swapPolicy, 11}, {"token_2022", siblingPolicy, 12}} {
		if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,active,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,$5,0,$6,$3,$7,$8,$9,$10,true,true,'create','finalized',999,'local-swap')`, cluster, settings, signer, shard.seed, shard.account, vault, shard.name, binding.Swap.MaxSlippageBPS, int64(binding.Swap.DailySourceMintSpendingCap), binding.Swap.ManifestFingerprint); err != nil {
			t.Fatal(err)
		}
	}
	seedConnectedLookupTable(t, ctx, store, cluster, signer, vaultID, sharedTable, sharedAddresses, "shared_market", "shared_market")
	seedConnectedLookupTable(t, ctx, store, cluster, signer, vaultID, vaultTable, vaultAddresses, "vault_shards", "vault_shard")
	position, err := store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Cluster: cluster, Slot: 1000, ObservedAt: now, Reserves: states}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	snapshot.Hash = epoch.Fingerprint
	snapshot.ExpiresAt = epoch.ExpiresAt
	snapshot.MintExpiresAt = map[string]time.Time{USDCMint: epoch.OptimizerEnvelopeExpiresAt(), USDTMint: epoch.OptimizerEnvelopeExpiresAt()}
	snapshot.OptimizerEpochID, err = store.EnsureOptimizerEpoch(ctx, cluster, epoch)
	if err != nil {
		t.Fatal(err)
	}
	fleetVault := FleetVault{Position: position, CrossMintTargets: map[string]CrossMintPolicyBindings{target.Address: binding}, CrossMintMaxValueLossBPS: 50}
	if sameMint {
		fleetVault.CrossMintTargets = nil
		fleetVault.AllowedTargets = []string{target.Address}
	}
	plan, err := PlanFleet(snapshot, []FleetVault{fleetVault})
	if err != nil || len(plan.Opportunities) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	published, err := store.Publish(ctx, cluster, epoch, position, plan.Opportunities[0].Decision)
	if err != nil || !published.Inserted {
		t.Fatalf("publish: %+v %v", published, err)
	}
	if err = store.RefreshTargetCapacity(ctx, cluster, target.Address, target.Mint, states[target.Address].TotalSupplyUSDMicros, 1000); err != nil {
		t.Fatal(err)
	}
	if sameMint {
		var initialPlan []byte
		if err = store.pool.QueryRow(ctx, `SELECT execution_plan FROM loyal_yield.rebalance_opportunities WHERE id=$1`, published.OpportunityID).Scan(&initialPlan); err != nil {
			t.Fatal(err)
		}
		runConnectedRustWorker(t, ctx, databaseURL, map[string]any{"setupOnly": true, "sameMint": true, "fixturePolicyBindings": binding, "cluster": cluster, "opportunityId": published.OpportunityID, "epochId": published.EpochID, "executionPlan": json.RawMessage(initialPlan), "rpcUrl": workerRPC.URL})
	}
	revalidator, err := NewRevalidator(store, NewRPCClient(server.URL), proxy, RevalidatorConfig{Owner: "connected-go", DelegatedSigner: signer, LeaseTTL: time.Minute, SlotDuration: 400 * time.Millisecond, CrossMintEnabled: true, CrossMintMaxValueLossBPS: 50, CrossMintMaxSlippageBPS: 50, JupiterBuildURL: server.URL + "/build"})
	if err != nil {
		t.Fatal(err)
	}
	// Trust only this local test server while preserving production timeouts
	// and the Jupiter client's same-origin redirect policy.
	revalidator.rpc.client.Transport = server.Client().Transport
	revalidator.jupiter.client.Transport = server.Client().Transport
	claimed, err := revalidator.Cycle(ctx, cluster)
	if err != nil || !claimed {
		t.Fatalf("cycle claimed=%v error=%v", claimed, err)
	}
	var state string
	var persisted []byte
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,execution_plan FROM loyal_yield.rebalance_opportunities WHERE id=$1`, published.OpportunityID).Scan(&state, &persisted); err != nil {
		t.Fatal(err)
	}
	if (!sameMint && (state != "ready" || simulated.Load() != 1 || !bytes.Contains(persisted, []byte("cross_mint_preflight")))) || (sameMint && state != "leased" && state != "ready") {
		t.Fatalf("incomplete preflight state=%s simulations=%d plan=%s", state, simulated.Load(), persisted)
	}
	if claimed, err = revalidator.Cycle(ctx, cluster); err != nil || claimed {
		t.Fatalf("ready work was reclaimed by revalidator: %v %v", claimed, err)
	}
	runConnectedRustWorker(t, ctx, databaseURL, map[string]any{"sameMint": sameMint, "fixturePolicyBindings": binding, "cluster": cluster, "opportunityId": published.OpportunityID, "epochId": published.EpochID, "executionPlan": json.RawMessage(persisted), "rpcUrl": workerRPC.URL})
	if artifact := os.Getenv("KAMINO_CONNECTED_PREFLIGHT_ARTIFACT"); artifact != "" {
		relative, err := filepath.Rel(os.TempDir(), artifact)
		if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			t.Fatal("artifact must be in disposable temp directory")
		}
		output, err := json.Marshal(map[string]any{"cluster": cluster, "opportunityId": published.OpportunityID, "epochId": published.EpochID, "executionPlan": json.RawMessage(persisted), "accounts": accounts, "signer": signer, "settings": settings, "vault": vault})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(artifact, output, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
