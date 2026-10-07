package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
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

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

type connectedAdmission struct {
	Admission fleet.ExecutionAdmission `json:"admission"`
	RPCURL    string                   `json:"rpcUrl"`
}

// This proves CURRENT Go C -> CURRENT Go D, not the retained Rust D lifecycle.
// C keeps its real Squads + explicit mock KLend SBF bank alive across the
// test-only callback. Only actual SBF execution changes financial accounts.
func TestConnectedGoSameMintExecution(t *testing.T) {
	database := os.Getenv("FLEET_TEST_GO_SAME_MINT_DATABASE_URL")
	if database == "" {
		t.Skip("requires the registered dedicated connected Go fixture")
	}
	dbURL, err := url.Parse(database)
	if err != nil || dbURL.Scheme != "postgresql" || dbURL.Hostname() != "127.0.0.1" || dbURL.Port() != "51913" || dbURL.User == nil || dbURL.User.Username() != "workers_v2" || (dbURL.Path != "/fleet_go_same_mint" && dbURL.Path != "/fleet_go_same_mint_simplify") || dbURL.RawQuery != "" || dbURL.Fragment != "" {
		t.Fatal("refusing database outside the dedicated registered loopback fixture")
	}
	if _, hasPassword := dbURL.User.Password(); hasPassword {
		t.Fatal("fixture database URL must not contain a password")
	}
	producerPath := os.Getenv("KAMINO_CONNECTED_GO_PLANNER_PATH")
	if producerPath == "" {
		t.Fatal("configured connected Go proof lacks current compiled C producer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(database)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var actualDB, role string
	if err := pool.QueryRow(ctx, `SELECT current_database(),current_user`).Scan(&actualDB, &role); err != nil || actualDB != dbURL.Path[1:] || role != "workers_v2" {
		t.Fatalf("unexpected fixture identity %q/%q: %v", actualDB, role, err)
	}
	store, err := NewStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var capability [32]byte
	if _, err := rand.Read(capability[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(capability[:])
	offers, finished := make(chan connectedAdmission, 1), make(chan bool, 1)
	defer func() {
		select {
		case finished <- false:
		default:
		}
	}()
	callback := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/handoff" || subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Connected-Fixture")), []byte(token)) != 1 {
			http.Error(out, "fixture capability rejected", http.StatusForbidden)
			return
		}
		var handoff connectedAdmission
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&handoff); err != nil {
			http.Error(out, "invalid handoff", http.StatusBadRequest)
			return
		}
		select {
		case offers <- handoff:
		case <-ctx.Done():
			return
		}
		select {
		case ok := <-finished:
			if !ok {
				http.Error(out, "terminal proof failed", http.StatusConflict)
				return
			}
			_ = json.NewEncoder(out).Encode(map[string]bool{"reconciled": true})
		case <-ctx.Done():
			http.Error(out, "fixture deadline", http.StatusRequestTimeout)
		}
	}))
	defer callback.Close()
	commandCtx, stopProducer := context.WithCancel(ctx)
	defer stopProducer()
	command := exec.CommandContext(commandCtx, producerPath, "-test.run=^TestConnectedGoSameMintAdmissionProducer$", "-test.v", "-test.timeout=110s")
	command.Env = []string{"LC_ALL=C", "FLEET_TEST_GO_SAME_MINT_DATABASE_URL=" + database, "KAMINO_CONNECTED_GO_D_CALLBACK=" + callback.URL + "/handoff", "KAMINO_CONNECTED_GO_D_TOKEN=" + token}
	for _, name := range []string{"KAMINO_TEST_KLEND_PROXY_PATH", "KAMINO_CONNECTED_SVM_PATH", "KAMINO_CONNECTED_WORKER_PATH", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO"} {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("configured proof lacks local artifact %s", name)
		}
		command.Env = append(command.Env, name+"="+value)
	}
	if path := os.Getenv("SQUADS_SMART_ACCOUNT_PROGRAM_SO"); path != "" {
		command.Env = append(command.Env, "SQUADS_SMART_ACCOUNT_PROGRAM_SO="+path)
	}
	var producerOutput bytes.Buffer
	command.Stdout, command.Stderr = &producerOutput, &producerOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	producerDone := make(chan error, 1)
	go func() { producerDone <- command.Wait() }()
	producerJoined := false
	defer func() {
		cancel()
		stopProducer()
		callback.Close()
		if !producerJoined {
			select {
			case <-producerDone:
			case <-time.After(5 * time.Second):
				t.Error("local C producer did not join after cancellation")
			}
		}
	}()
	var handoff connectedAdmission
	select {
	case handoff = <-offers:
	case err := <-producerDone:
		producerJoined = true
		t.Fatalf("current C producer exited without admission: %v\n%s", err, producerOutput.String())
	case <-ctx.Done():
		t.Fatal("current C producer timed out before live admission")
	}
	rpcURL, err := url.Parse(handoff.RPCURL)
	if err != nil || rpcURL.Scheme != "http" || rpcURL.Hostname() != "127.0.0.1" || rpcURL.User != nil || rpcURL.RawQuery != "" || rpcURL.Fragment != "" {
		t.Fatal("producer changed local RPC scope")
	}
	a := handoff.Admission
	if a.Lease.Cluster != "localnet" || a.Lease.Owner != "connected-go-d" || a.Lease.VaultIndex != 1 || a.Lease.RouteKind != "same_mint" {
		t.Fatal("C did not supply canonical classic Earn admission")
	}
	adapter, err := NewRPCAdapter(handoff.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	land, err := solana.NewLandRPC(handoff.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workerConfig := Config{Cluster: a.Lease.Cluster, Owner: a.Lease.Owner, LeaseTTL: 5 * time.Second, BatchSize: 1, TickInterval: 20 * time.Millisecond, SlotDuration: 400 * time.Millisecond, Facts: testFacts()}
	signer := DelegateSigner{FeePayer: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))}
	worker, err := NewWorker(workerConfig, store, land, adapter, signer)
	if err != nil {
		t.Fatal(err)
	}
	id, err := worker.ExecuteFresh(ctx, a)
	if err != nil {
		t.Fatalf("Go D refused actual C admission: %v", err)
	}
	var signature, messageHash, signedHash, state string
	var originalWire []byte
	var broadcasts int
	var decisionID int64
	if err := pool.QueryRow(ctx, `SELECT transaction_signature,message_hash,signed_transaction_hash,signed_transaction,submission_state,broadcast_count,decision_id FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&signature, &messageHash, &signedHash, &originalWire, &state, &broadcasts, &decisionID); err != nil {
		t.Fatal(err)
	}
	if state != "signed" || broadcasts != 0 || decisionID <= 0 || messageHash != a.Preparation.Transaction.MessageSHA256 {
		t.Fatal("wire was not durably bound before broadcast")
	}
	verifyConnectedPolicyIndex(t, originalWire, a)
	var reserved, conflicts int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE id=$1 AND signed_submission_id=$2 AND decision_id=$3 AND reservation_state<>'released'),(SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$4 AND submission_id=$2)`, a.CapacityReservationID, id, decisionID, a.Lease.OpportunityID).Scan(&reserved, &conflicts); err != nil || reserved != 1 || conflicts != len(a.ConflictKeys) {
		t.Fatalf("signed ownership not retained capacity=%d conflicts=%d: %v", reserved, conflicts, err)
	}
	// The fixture executes the send but loses its response; landing reads the
	// confirmed signature and hands off reconciliation without another send.
	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("initial Go landing: %v", err)
	}
	worker.wg.Wait()
	if err := pool.QueryRow(ctx, `SELECT submission_state,broadcast_count FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&state, &broadcasts); err != nil || state != "reconciliation_pending" || broadcasts != 1 {
		t.Fatalf("executed response loss did not land %s/%d: %v", state, broadcasts, err)
	}
	// The chain moves its frontier, not its balances, through this fixture RPC.
	var advanced int64
	if err := adapter.call(ctx, &advanced, "advanceSlot", int64(1001)); err != nil || advanced != 1001 {
		t.Fatalf("actual SVM frontier: %d %v", advanced, err)
	}
	workerConfig.Owner = "connected-go-d-restarted"
	// Recovery has no key at all. A successful terminal result cannot re-sign.
	restarted, err := NewWorker(workerConfig, store, land, adapter, DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	for state != "reconciled" {
		if err := restarted.Tick(ctx); err != nil {
			t.Fatalf("live Go recovery/reconciliation: %v", err)
		}
		restarted.wg.Wait()
		if err := pool.QueryRow(ctx, `SELECT submission_state FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "failed" || state == "expired" {
			t.Fatalf("actual executed route became %s", state)
		}
		if state != "reconciled" {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("Go recovery did not terminate")
			}
		}
	}
	var finalWire []byte
	var finalSignature, finalMessage, finalHash, opportunity, decision string
	var postSnapshot, postSlot int64
	if err := pool.QueryRow(ctx, `SELECT s.signed_transaction,s.transaction_signature,s.message_hash,s.signed_transaction_hash,s.broadcast_count,o.opportunity_state,d.status::text,d.post_snapshot_id,s.reconciled_slot FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id WHERE s.id=$1`, id).Scan(&finalWire, &finalSignature, &finalMessage, &finalHash, &broadcasts, &opportunity, &decision, &postSnapshot, &postSlot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalWire, finalWire) || signature != finalSignature || messageHash != finalMessage || signedHash != finalHash || broadcasts != 1 || opportunity != "completed" || decision != "confirmed" || postSnapshot <= 0 || postSlot != 1001 {
		t.Fatal("recovery changed immutable wire or failed terminal publication")
	}
	var sourceRaw, targetRaw int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$2),(SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$3)`, a.Lease.VaultID, a.Anchors.SourceReserve, a.Anchors.TargetReserve).Scan(&sourceRaw, &targetRaw); err != nil || sourceRaw != 0 || targetRaw != int64(a.Lease.LiquidityAmountRaw) {
		t.Fatalf("actual SBF collateral effects were not published source=%d target=%d: %v", sourceRaw, targetRaw, err)
	}
	if n, err := store.ReleaseTelemetryReflectedCapacity(ctx, a.Lease.Cluster); err != nil || n != 0 {
		t.Fatalf("capacity released before actual newer telemetry %d %v", n, err)
	}
	chain := fleet.NewRPCClient(handoff.RPCURL)
	slot, accounts, err := chain.FinalizedAccounts(ctx, []string{a.Anchors.TargetReserve}, 1001)
	if err != nil || len(accounts) != 1 || slot != 1001 {
		t.Fatalf("actual target telemetry %d %v", slot, err)
	}
	targetState, err := fleet.DecodeKaminoReserve(accounts[0], fleet.ReserveIdentity{Address: a.Anchors.TargetReserve, Market: a.Anchors.TargetMarket, Mint: a.Anchors.Mint}, slot, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	plannerStore, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := plannerStore.RefreshTargetCapacity(ctx, a.Lease.Cluster, a.Anchors.TargetReserve, a.Anchors.Mint, targetState.TotalSupplyUSDMicros, slot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseTelemetryReflectedCapacity(ctx, a.Lease.Cluster); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND reservation_state<>'released'),(SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1)`, a.Lease.OpportunityID).Scan(&reserved, &conflicts); err != nil || reserved != 0 || conflicts != 0 {
		t.Fatalf("terminal ownership capacity=%d conflicts=%d: %v", reserved, conflicts, err)
	}
	finished <- true
	select {
	case err := <-producerDone:
		producerJoined = true
		if err != nil {
			t.Fatalf("current C producer failed: %v\n%s", err, producerOutput.String())
		}
	case <-ctx.Done():
		t.Fatal("C producer did not join")
	}
	t.Logf("GO_D_CONNECTED_SVM_EVIDENCE opportunity=%d submission=%d signature=%s vault_index=1 broadcasts=1 collateral_source=0 collateral_target=%d post_slot=%d capacity=0 conflicts=0 programs=actual_squads,explicit_mock_klend", a.Lease.OpportunityID, id, signature, targetRaw, postSlot)
}

func verifyConnectedPolicyIndex(t *testing.T, wire []byte, admission fleet.ExecutionAdmission) {
	t.Helper()
	tx, err := sdk.TransactionFromBytes(wire)
	if err != nil || tx.VerifySignatures() != nil {
		t.Fatalf("current Go D did not sign valid canonical wire: %v", err)
	}
	tables := map[sdk.PublicKey]sdk.PublicKeySlice{}
	for _, table := range admission.SelectedALTs {
		for _, address := range table.Addresses {
			tables[sdk.MustPublicKeyFromBase58(table.Address)] = append(tables[sdk.MustPublicKeyFromBase58(table.Address)], sdk.MustPublicKeyFromBase58(address))
		}
	}
	if err := tx.Message.SetAddressTables(tables); err != nil {
		t.Fatal(err)
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		t.Fatal(err)
	}
	found := 0
	constraints := map[byte]bool{}
	for _, instruction := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(instruction.ProgramIDIndex)
		if err != nil {
			t.Fatal(err)
		}
		if program.String() == fleet.SquadsProgram {
			found++
			data := []byte(instruction.Data)
			// The pinned Rust SDK serializer has account_index, num_signers,
			// Policy, ProgramInteraction, Some(indices), then SyncTransaction.
			// Mature same-mint execution wraps withdrawal and deposit separately.
			if len(data) < 25 || !bytes.Equal(data[:8], []byte{90, 81, 187, 81, 39, 70, 128, 78}) || !bytes.Equal(data[8:13], []byte{1, 1, 1, 1, 1}) || binary.LittleEndian.Uint32(data[13:17]) != 1 || data[17] > 1 || constraints[data[17]] || data[18] != 1 || data[19] != 1 || int(binary.LittleEndian.Uint32(data[20:24])) != len(data)-24 || data[24] != 1 {
				t.Fatal("Go D signed a different vault, policy constraint, or SDK payload")
			}
			constraints[data[17]] = true
			if len(instruction.Accounts) < 3 || len(admission.Lease.DelegatedSigners) != 1 || tx.Message.AccountKeys[instruction.Accounts[0]].String() != admission.Lease.PolicyAccount || tx.Message.AccountKeys[instruction.Accounts[2]].String() != admission.Lease.DelegatedSigners[0] {
				t.Fatal("Go D signed a different policy or delegate binding")
			}
		}
	}
	if found != 2 || !constraints[0] || !constraints[1] {
		t.Fatalf("expected both canonical Squads same-mint wrappers, got %d", found)
	}
}
