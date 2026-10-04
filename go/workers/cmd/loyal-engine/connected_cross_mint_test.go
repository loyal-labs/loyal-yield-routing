package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
)

type retailConnectedCrossMintBank struct {
	RPCURL, BuildURL, Cluster string
	TLSCA                     []byte
	OpportunityID, EpochID    int64
	VaultID                   int64
	VaultIndex                uint8
	Vault, Settings, Signer   string
	Source, Target            fleet.KaminoPositionAccounts
}

type retailConnectedPublishedEpoch struct{ epoch fleet.ImmutableMarketEpoch }

func (s retailConnectedPublishedEpoch) LoadImmutableMarketEpoch(ctx context.Context) (fleet.ImmutableMarketEpoch, error) {
	return s.epoch, ctx.Err()
}

// CURRENT retail adapter -> CURRENT C verifier -> CURRENT D custody owner.
// The child owns the live SVM bank only; no test financial factory, synthetic
// admission, caller-certified receipt, or retained Rust execution substitutes
// for the actual Go paths. KLend and Jupiter are explicitly mock SBF programs.
func TestConnectedRetailGoCrossMintExecution(t *testing.T) {
	database := os.Getenv("FLEET_TEST_GO_CROSS_MINT_DATABASE_URL")
	if database == "" {
		t.Skip("requires registered dedicated current Go cross-mint fixture")
	}
	u, err := url.Parse(database)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || u.Port() != "51913" || u.User == nil || u.User.Username() != "workers_v2" || u.Path != "/fleet_go_cross_mint" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("refusing unregistered cross-mint fixture database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	poolConfig, err := pgxpool.ParseConfig(database)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var db, role, address string
	if err := pool.QueryRow(ctx, `SELECT current_database(),current_user,host(inet_server_addr())`).Scan(&db, &role, &address); err != nil || db != "fleet_go_cross_mint" || role != "workers_v2" || address != "127.0.0.1" {
		t.Fatalf("unexpected actual fixture identity %q/%q/%q: %v", db, role, address, err)
	}
	store, err := fleetexec.NewStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	plannerStore, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	var capability [32]byte
	if _, err := rand.Read(capability[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(capability[:])
	offers, completed := make(chan retailConnectedCrossMintBank, 1), make(chan bool, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/bank" || subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Connected-Fixture")), []byte(token)) != 1 {
			http.Error(out, "fixture capability rejected", http.StatusForbidden)
			return
		}
		var bank retailConnectedCrossMintBank
		if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&bank) != nil {
			http.Error(out, "invalid fixture metadata", http.StatusBadRequest)
			return
		}
		select {
		case offers <- bank:
		case <-ctx.Done():
			return
		}
		select {
		case ok := <-completed:
			if !ok {
				http.Error(out, "terminal proof failed", http.StatusConflict)
				return
			}
			_ = json.NewEncoder(out).Encode(map[string]bool{"Reconciled": true})
		case <-ctx.Done():
			http.Error(out, "fixture deadline", http.StatusRequestTimeout)
		}
	}))
	defer callback.Close()
	producerPath := os.Getenv("KAMINO_CONNECTED_GO_PLANNER_PATH")
	if producerPath == "" {
		t.Fatal("configured proof lacks current compiled C bank producer")
	}
	child := exec.CommandContext(ctx, producerPath, "-test.run=^TestConnectedGoCrossMintBankProducer$", "-test.v", "-test.timeout=115s")
	child.Env = []string{"LC_ALL=C", "FLEET_TEST_GO_CROSS_MINT_DATABASE_URL=" + database, "KAMINO_CONNECTED_GO_CROSS_MINT_CALLBACK=" + callback.URL + "/bank", "KAMINO_CONNECTED_GO_CROSS_MINT_TOKEN=" + token}
	for _, name := range []string{"KAMINO_TEST_KLEND_PROXY_PATH", "KAMINO_CONNECTED_SVM_PATH", "KAMINO_CONNECTED_WORKER_PATH", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO"} {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("configured proof lacks local artifact %s", name)
		}
		child.Env = append(child.Env, name+"="+value)
	}
	if path := os.Getenv("SQUADS_SMART_ACCOUNT_PROGRAM_SO"); path != "" {
		child.Env = append(child.Env, "SQUADS_SMART_ACCOUNT_PROGRAM_SO="+path)
	}
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() {
		cancel()
		callback.Close()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("current C bank producer did not join after cancellation")
			}
		}
	}()
	var bank retailConnectedCrossMintBank
	select {
	case bank = <-offers:
	case err := <-done:
		joined = true
		t.Fatalf("C exited without a live bank: %v\n%s", err, output.String())
	case <-ctx.Done():
		t.Fatal("C bank startup timed out")
	}
	for raw, scheme := range map[string]string{bank.RPCURL: "http", bank.BuildURL: "https"} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != scheme || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatal("C changed the local chain/build origin")
		}
	}
	if bank.Cluster != "localnet" || bank.VaultIndex != 1 || bank.OpportunityID <= 0 || bank.EpochID <= 0 || bank.VaultID <= 0 || bank.Source.LiquidityMint != fleet.USDCMint || bank.Target.LiquidityMint != fleet.USDTMint {
		t.Fatal("C did not offer canonical classic Earn cross-mint work")
	}
	// Only the offered fixture CA is trusted, and only for this isolated test's
	// clients. HTTPS and Jupiter's same-origin redirect checks stay intact.
	trusted := x509.NewCertPool()
	if !trusted.AppendCertsFromPEM(bank.TLSCA) {
		t.Fatal("C omitted its public fixture CA")
	}
	originalTransport := http.DefaultTransport
	transport := originalTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: trusted, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	defer func() { transport.CloseIdleConnections(); http.DefaultTransport = originalTransport }()
	proxyPath := os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	proxyBytes, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	proxyHash := sha256.Sum256(proxyBytes)
	proxy, err := fleet.NewKLendProxy(proxyPath, hex.EncodeToString(proxyHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	owner := "connected-retail-crossmint"
	revalidator, err := fleet.NewRevalidator(plannerStore, fleet.NewRPCClient(bank.RPCURL), proxy, fleet.RevalidatorConfig{Owner: owner, DelegatedSigner: bank.Signer, LeaseTTL: time.Minute, SlotDuration: 400 * time.Millisecond, CrossMintEnabled: true, CrossMintMaxValueLossBPS: 50, CrossMintMaxSlippageBPS: 50, JupiterBuildURL: bank.BuildURL})
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := newRetailCrossMintAdapters(revalidator)
	if err != nil {
		t.Fatal(err)
	}
	rpc, err := fleetexec.NewRPCAdapter(bank.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config := fleetexec.Config{Cluster: bank.Cluster, Owner: owner, LeaseTTL: 20 * time.Second, BatchSize: 1, TickInterval: 20 * time.Millisecond, SlotDuration: 400 * time.Millisecond}
	signer := fleetexec.DelegateSigner{FeePayer: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))}
	controller, err := fleetexec.NewCrossMintController(store, adapters, signer, rpc, bank.Cluster, owner, config.LeaseTTL, true)
	if err != nil {
		t.Fatal(err)
	}
	var epochJSON []byte
	var actualEpoch fleet.ImmutableMarketEpoch
	if err := pool.QueryRow(ctx, `SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1 AND cluster=$2`, bank.EpochID, bank.Cluster).Scan(&epochJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(epochJSON, &actualEpoch); err != nil {
		t.Fatal(err)
	}
	if err := actualEpoch.Validate(); err != nil {
		t.Fatalf("actual C-published market epoch: %v", err)
	}
	controller.SetMarketEpochSource(retailConnectedPublishedEpoch{actualEpoch})
	runtime, err := fleetexec.NewCrossMintRuntime(ctx, config, store, controller, rpc, adapters)
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetActivationSource(adapters)
	for phase := 0; phase < 2; phase++ {
		if n, err := runtime.Tick(ctx); err != nil || n != 1 {
			t.Fatalf("actual activation/signed publication phase%d n=%d: %v", phase, n, err)
		}
	}
	var submissionID, decisionID int64
	var originalWire []byte
	var signature, wireHash, state string
	var broadcasts int
	if err := pool.QueryRow(ctx, `SELECT id,decision_id,signed_transaction,transaction_signature,signed_transaction_hash,submission_state,broadcast_count FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1 AND movement_leg='withdraw'`, bank.OpportunityID).Scan(&submissionID, &decisionID, &originalWire, &signature, &wireHash, &state, &broadcasts); err != nil || state != "signed" || broadcasts != 0 {
		t.Fatalf("withdrawal was not persisted before send: %s/%d %v", state, broadcasts, err)
	}
	verifyRetailConnectedVault(t, ctx, pool, originalWire, bank.Cluster)
	if n, err := runtime.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("first actual withdrawal send n=%d: %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT submission_state,broadcast_count FROM loyal_yield.signed_route_submissions WHERE id=$1`, submissionID).Scan(&state, &broadcasts); err != nil || state != "effect_ambiguous" || broadcasts != 1 {
		t.Fatalf("executed response loss was not retained: %s/%d %v", state, broadcasts, err)
	}
	// This recovery owner receives no controller, activation source or key.
	recoveryConfig := config
	recoveryConfig.Owner = owner + "-recovery"
	recovery, err := fleetexec.NewCrossMintRecoveryRuntime(ctx, recoveryConfig, store, rpc, adapters)
	if err != nil {
		t.Fatalf("concrete signerless recovery API required: %v", err)
	}
	for state != "reconciled" {
		if _, err := recovery.Tick(ctx); err != nil {
			t.Fatalf("signerless original-wire recovery: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT submission_state FROM loyal_yield.signed_route_submissions WHERE id=$1`, submissionID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "reconciled" {
			connectedRetailPause(t, ctx)
		}
	}
	var recoveredWire []byte
	var recoveredSignature, recoveredHash string
	if err := pool.QueryRow(ctx, `SELECT signed_transaction,transaction_signature,signed_transaction_hash,broadcast_count FROM loyal_yield.signed_route_submissions WHERE id=$1`, submissionID).Scan(&recoveredWire, &recoveredSignature, &recoveredHash, &broadcasts); err != nil || !bytes.Equal(originalWire, recoveredWire) || signature != recoveredSignature || wireHash != recoveredHash || broadcasts != 1 {
		t.Fatalf("signerless recovery substituted or resent the wire: %v", err)
	}
	var terminal *string
	for terminal == nil {
		if _, err := runtime.Tick(ctx); err != nil {
			t.Fatalf("actual swap/deposit continuation: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT terminal_outcome FROM loyal_yield.rebalance_decisions WHERE id=$1`, decisionID).Scan(&terminal); err != nil {
			t.Fatal(err)
		}
		if terminal == nil {
			connectedRetailPause(t, ctx)
		}
	}
	if *terminal != "completed_target" {
		t.Fatalf("actual Go custody ended as %s", *terminal)
	}
	verifyRetailConnectedTerminal(t, ctx, pool, store, plannerStore, bank, decisionID)
	completed <- true
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatalf("bank producer failed its send-count proof: %v\n%s", err, output.String())
		}
	case <-ctx.Done():
		t.Fatal("C bank did not join after terminal proof")
	}
	t.Logf("GO_RETAIL_CROSS_MINT_SVM_EVIDENCE opportunity=%d decision=%d withdrawal=%d signature=%s vault_index=1 programs=actual_squads,explicit_mock_klend,explicit_mock_jupiter", bank.OpportunityID, decisionID, submissionID, signature)
}

func connectedRetailPause(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("current Go cross-mint lifecycle timed out")
	}
}

func verifyRetailConnectedVault(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wire []byte, cluster string) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil || tx.VerifySignatures() != nil {
		t.Fatalf("actual Go cross-mint wire is invalid: %v", err)
	}
	tables := map[solana.PublicKey]solana.PublicKeySlice{}
	rows, err := pool.Query(ctx, `SELECT table_address,array_agg(a.address ORDER BY a.ordinal) FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_addresses a ON a.route_lookup_table_id=t.id WHERE t.cluster=$1 GROUP BY t.id`, cluster)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var address string
		var members []string
		if err := rows.Scan(&address, &members); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for _, member := range members {
			tables[solana.MustPublicKeyFromBase58(address)] = append(tables[solana.MustPublicKeyFromBase58(address)], solana.MustPublicKeyFromBase58(member))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Message.SetAddressTables(tables); err != nil {
		t.Fatal(err)
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, instruction := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(instruction.ProgramIDIndex)
		if err != nil {
			t.Fatal(err)
		}
		if program.String() == fleet.SquadsProgram {
			data := []byte(instruction.Data)
			if len(data) < 13 || !bytes.Equal(data[:8], []byte{90, 81, 187, 81, 39, 70, 128, 78}) || !bytes.Equal(data[8:13], []byte{1, 1, 1, 1, 1}) {
				t.Fatal("actual wire changed canonical vault1 SDK Policy envelope")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("actual wire omitted Squads execution")
	}
}

func verifyRetailConnectedTerminal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *fleetexec.Store, planner *fleet.Store, bank retailConnectedCrossMintBank, decisionID int64) {
	t.Helper()
	var custody, version int64
	var status, opportunity string
	var total, reconciled, once int
	if err := pool.QueryRow(ctx, `SELECT d.custody_amount_raw,d.custody_version,d.status::text,o.opportunity_state,(SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),(SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND submission_state='reconciled'),(SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND broadcast_count=1) FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id WHERE d.id=$1`, decisionID).Scan(&custody, &version, &status, &opportunity, &total, &reconciled, &once); err != nil || custody != 0 || version != 3 || status != "confirmed" || opportunity != "completed" || total != 3 || reconciled != 3 || once != 3 {
		t.Fatalf("actual terminal custody/receipt identity failed: custody%d version%d %s/%s legs%d/%d/%d: %v", custody, version, status, opportunity, total, reconciled, once, err)
	}
	// Provider inputs stay distinct from managed table IDs even when the
	// compiler selects only managed tables for this small fixture packet.
	var journalJSON, swapWire []byte
	if err := pool.QueryRow(ctx, `SELECT alt_mutation_epochs,signed_transaction FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND movement_leg='swap' AND submission_state='reconciled'`, decisionID).Scan(&journalJSON, &swapWire); err != nil {
		t.Fatal(err)
	}
	var journal struct {
		Tables            []json.RawMessage            `json:"tables"`
		ExternalSnapshots []fleet.CrossMintExternalALT `json:"externalSnapshots"`
		LookupOrder       []string                     `json:"lookupTableOrder"`
	}
	if err := json.Unmarshal(journalJSON, &journal); err != nil || len(journal.Tables) != 2 || len(journal.ExternalSnapshots) != 1 || len(journal.LookupOrder) != 2 {
		t.Fatalf("actual mixed ALT journal lost managed/provider separation: %v", err)
	}
	swapTransaction, err := solana.TransactionFromBytes(swapWire)
	if err != nil || swapTransaction.VerifySignatures() != nil || len(swapTransaction.Message.AddressTableLookups) != len(journal.LookupOrder) {
		t.Fatalf("actual swap wire lookup proof: %v", err)
	}
	for i, table := range swapTransaction.Message.AddressTableLookups {
		if table.AccountKey.String() != journal.LookupOrder[i] {
			t.Fatal("durable mixed ALT order differs from actual signed wire")
		}
	}
	external := journal.ExternalSnapshots[0]
	var managed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE cluster=$1 AND table_address=$2`, bank.Cluster, external.Address).Scan(&managed); err != nil || managed != 0 {
		t.Fatalf("provider input acquired fabricated managed table identity: %d %v", managed, err)
	}
	chain := fleet.NewRPCClient(bank.RPCURL)
	providerSlot, providerAccounts, err := chain.FinalizedAccounts(ctx, []string{external.Address}, external.ObservedSlot)
	if err != nil || providerSlot < external.ObservedSlot || len(providerAccounts) != 1 {
		t.Fatalf("actual finalized provider ALT snapshot unavailable: %v", err)
	}
	provider := providerAccounts[0]
	if provider.Address != external.Address || provider.Owner != solana.AddressLookupTableProgramID.String() || len(provider.Data) != 56+32*len(external.Addresses) || binary.LittleEndian.Uint32(provider.Data[:4]) != 1 || binary.LittleEndian.Uint64(provider.Data[4:12]) != ^uint64(0) || external.ObservedSlot <= external.UsableAfterSlot {
		t.Fatal("actual finalized provider ALT identity/member bounds differ")
	}
	for i, member := range external.Addresses {
		if solana.PublicKeyFromBytes(provider.Data[56+32*i:56+32*(i+1)]).String() != member {
			t.Fatal("durable provider member vector differs from actual finalized bytes")
		}
	}
	hash, err := fleet.CrossMintExternalAddressHash(external.Addresses)
	if err != nil || hash != external.OrderedAddressHash {
		t.Fatalf("durable provider ordered member hash changed: %v", err)
	}
	// Execution owns receipt/custody evidence; the retained observer owns its
	// current-position projection. Verify actual finalized obligations instead
	// of requiring that independent observer to have run inside this fixture.
	_, obligations, err := chain.FinalizedAccounts(ctx, []string{bank.Source.Obligation, bank.Target.Obligation}, 1000)
	if err != nil || len(obligations) != 2 {
		t.Fatalf("actual finalized terminal obligations: %v", err)
	}
	source := retailConnectedCollateral(t, obligations[0], bank.Source, bank.Vault)
	target := retailConnectedCollateral(t, obligations[1], bank.Target, bank.Vault)
	var swapCredit int64
	var withdrawnJSON, depositedJSON []byte
	if err := pool.QueryRow(ctx, `SELECT (SELECT (reconciled_effect->'credit'->>'amountRaw')::bigint FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND movement_leg='swap'),(SELECT reconciled_balance_anchors FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND movement_leg='withdraw'),(SELECT reconciled_balance_anchors FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND movement_leg='deposit')`, decisionID).Scan(&swapCredit, &withdrawnJSON, &depositedJSON); err != nil {
		t.Fatal(err)
	}
	var withdrawn, deposited fleetexec.CrossMintBalanceAnchors
	if err := json.Unmarshal(withdrawnJSON, &withdrawn); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(depositedJSON, &deposited); err != nil {
		t.Fatal(err)
	}
	if source != 1 || target == 0 || target != uint64(swapCredit) || withdrawn.Position == nil || deposited.Position == nil || withdrawn.Position.Reserve != bank.Source.Reserve || withdrawn.Position.Obligation != bank.Source.Obligation || withdrawn.Position.Market != bank.Source.Market || withdrawn.Position.CollateralRaw != int64(source) || deposited.Position.Reserve != bank.Target.Reserve || deposited.Position.Obligation != bank.Target.Obligation || deposited.Position.Market != bank.Target.Market || deposited.Position.CollateralRaw != int64(target) {
		t.Fatalf("actual 1:1 mock-KLend collateral differs from exact saved position/swap receipts: %d/%d/%d", source, target, swapCredit)
	}
	if n, err := store.ReleaseTelemetryReflectedCapacity(ctx, bank.Cluster); err != nil || n != 0 {
		t.Fatalf("capacity released without newer actual reserve telemetry: %d %v", n, err)
	}
	var held int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND reservation_state='awaiting_telemetry'`, bank.OpportunityID).Scan(&held); err != nil || held != 1 {
		t.Fatalf("movement capacity disappeared before fresh telemetry: %d %v", held, err)
	}
	_, balances, err := chain.FinalizedAccounts(ctx, []string{bank.Source.VaultLiquidityATA, bank.Target.VaultLiquidityATA}, 1000)
	if err != nil || len(balances) != 2 {
		t.Fatalf("actual terminal token custody: %v", err)
	}
	for i, account := range balances {
		position := []fleet.KaminoPositionAccounts{bank.Source, bank.Target}[i]
		if account.Address != position.VaultLiquidityATA || account.Owner != position.LiquidityTokenProgram || len(account.Data) < 165 || solana.PublicKeyFromBytes(account.Data[:32]).String() != position.LiquidityMint || solana.PublicKeyFromBytes(account.Data[32:64]).String() != bank.Vault || binary.LittleEndian.Uint64(account.Data[64:72]) != 0 {
			t.Fatal("actual terminal token identity or zero-custody proof differs")
		}
	}
	var advanced int64
	connectedRetailRPC(t, ctx, bank.RPCURL, "advanceSlot", []any{int64(1001)}, &advanced)
	if advanced != 1001 {
		t.Fatal("actual SVM frontier did not advance")
	}
	slot, accounts, err := chain.FinalizedAccounts(ctx, []string{bank.Target.Reserve}, 1001)
	if err != nil || slot != 1001 || len(accounts) != 1 {
		t.Fatalf("actual target reserve telemetry: slot%d %v", slot, err)
	}
	reserve, err := fleet.DecodeKaminoReserve(accounts[0], fleet.ReserveIdentity{Address: bank.Target.Reserve, Market: bank.Target.Market, Mint: bank.Target.LiquidityMint}, slot, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.RefreshTargetCapacity(ctx, bank.Cluster, bank.Target.Reserve, bank.Target.LiquidityMint, reserve.TotalSupplyUSDMicros, slot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseTelemetryReflectedCapacity(ctx, bank.Cluster); err != nil {
		t.Fatal(err)
	}
	var capacity, conflicts int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND reservation_state<>'released'),(SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1)`, bank.OpportunityID).Scan(&capacity, &conflicts); err != nil || capacity != 0 || conflicts != 0 {
		t.Fatalf("actual terminal owned capacity/conflicts remained %d/%d: %v", capacity, conflicts, err)
	}
}

// KLend's pinned Obligation ABI: discriminator, market/owner, eight 136-byte
// deposit slots. This independent bank-byte check does not use D's post-state
// decoder or the asynchronous observer projection.
func retailConnectedCollateral(t *testing.T, account fleet.Account, position fleet.KaminoPositionAccounts, vault string) uint64 {
	t.Helper()
	if account.Address != position.Obligation || account.Owner != fleet.KLendProgram || len(account.Data) != 3344 || !bytes.Equal(account.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167}) || solana.PublicKeyFromBytes(account.Data[32:64]).String() != position.Market || solana.PublicKeyFromBytes(account.Data[64:96]).String() != vault {
		t.Fatal("actual finalized KLend obligation identity changed")
	}
	var collateral uint64
	found := false
	for i := 0; i < 8; i++ {
		offset := 96 + 136*i
		reserve := solana.PublicKeyFromBytes(account.Data[offset : offset+32])
		if reserve.IsZero() {
			continue
		}
		if found || reserve.String() != position.Reserve {
			t.Fatal("actual fixture obligation acquired an unrelated or duplicate deposit")
		}
		found = true
		collateral = binary.LittleEndian.Uint64(account.Data[offset+32 : offset+40])
	}
	if !found {
		t.Fatal("actual fixture terminal obligation lacks expected collateral")
	}
	return collateral
}

func connectedRetailRPC(t *testing.T, ctx context.Context, endpoint, method string, params []any, target any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct{ Result, Error json.RawMessage }
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil || len(envelope.Error) > 0 || len(envelope.Result) == 0 {
		t.Fatalf("actual fixture RPC %s failed", method)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatal(err)
	}
}
