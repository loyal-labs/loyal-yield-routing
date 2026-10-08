package fleet_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// Go plans, persists the signed wire before any send, lands it through its own
// land() against the local SVM (whose first executed send loses its response),
// and reconciles from the finalized receipt. No Rust process participates.
func TestConnectedSameMintExecution(t *testing.T) {
	runConnectedSameMint(t, fleet.ConnectedSameMint)
}

// The target market has no obligation and the vault holds no lamports: the
// same transaction withdraws, funds the vault's obligation rent, initializes
// the obligation under the setup policy, and deposits.
func TestConnectedSameMintSetupExecution(t *testing.T) {
	runConnectedSameMint(t, fleet.ConnectedSameMintSetup)
}

func runConnectedSameMint(t *testing.T, kind fleet.ConnectedKind) {
	bank := fleet.NewConnectedBank(t, kind)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	owner := "connected-same-mint"
	revalidator, err := bank.Revalidator(owner, true)
	if err != nil {
		t.Fatal(err)
	}
	store, err := fleetexec.NewStore(ctx, bank.Pool)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := fleetexec.NewRPCAdapter(bank.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	land, err := solana.NewLandRPC(bank.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config := fleetexec.Config{Cluster: bank.Cluster, Owner: owner, LeaseTTL: 20 * time.Second, BatchSize: 1, TickInterval: 20 * time.Millisecond, SlotDuration: 400 * time.Millisecond, Facts: engine.NewFacts(prometheus.NewRegistry())}
	worker, err := fleetexec.NewWorker(config, store, land, adapter, fleetexec.DelegateSigner{FeePayer: bank.Signer})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SetFreshRevalidator(revalidator); err != nil {
		t.Fatal(err)
	}
	// One tick prepares, signs and persists; nothing is sent before the row exists.
	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("fresh Go preparation: %v", err)
	}
	var id int64
	var state string
	var broadcasts int
	var wire []byte
	if err := bank.Pool.QueryRow(ctx, `SELECT id,submission_state,broadcast_count,signed_transaction FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1`, bank.OpportunityID).Scan(&id, &state, &broadcasts, &wire); err != nil {
		t.Fatal(err)
	}
	if state != "signed" || broadcasts != 0 || bank.Sends() != 0 {
		t.Fatalf("wire was not durable before send: %s/%d sends=%d", state, broadcasts, bank.Sends())
	}
	verifySameMintWire(t, ctx, bank, wire, kind)
	// The next tick lands the exact bytes; the chain executes the first send
	// but its response is lost, so landing confirms from the signature.
	state = waitSubmission(t, ctx, bank, worker, id, "reconciliation_pending")
	var advanced int64
	connectedRPC(t, ctx, bank.RPCURL, "advanceSlot", []any{int64(1001)}, &advanced)
	// A restarted owner without any key finishes reconciliation.
	config.Owner = owner + "-restarted"
	restarted, err := fleetexec.NewWorker(config, store, land, adapter, fleetexec.DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	waitSubmission(t, ctx, bank, restarted, id, "reconciled")
	var finalWire []byte
	var opportunity, decision string
	var sourceRaw, targetRaw int64
	if err := bank.Pool.QueryRow(ctx, `SELECT s.signed_transaction,s.broadcast_count,o.opportunity_state,d.status::text,
 (SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=o.vault_id AND reserve=o.source_reserve),
 (SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=o.vault_id AND reserve=o.target_reserve)
 FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id WHERE s.id=$1`, id).Scan(&finalWire, &broadcasts, &opportunity, &decision, &sourceRaw, &targetRaw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire, finalWire) || broadcasts != 1 || bank.Sends() != 1 || bank.LostResponses() != 1 || opportunity != "completed" || decision != "confirmed" || sourceRaw != 0 || targetRaw <= 0 {
		t.Fatalf("terminal state: broadcasts=%d sends=%d lost=%d %s/%s source=%d target=%d", broadcasts, bank.Sends(), bank.LostResponses(), opportunity, decision, sourceRaw, targetRaw)
	}
	// Independently of Go's reconciliation, the chain holds the collateral in
	// the target obligation the route created or reused.
	_, obligations, err := fleet.NewRPCClient(bank.RPCURL).FinalizedAccounts(ctx, []string{bank.Source.Obligation, bank.Target.Obligation}, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if source, target := obligationCollateral(t, obligations[0], bank.Vault), obligationCollateral(t, obligations[1], bank.Vault); source != 0 || int64(target) != targetRaw {
		t.Fatalf("chain collateral source=%d target=%d, published target=%d", source, target, targetRaw)
	}
}

type publishedEpoch struct{ epoch fleet.ImmutableMarketEpoch }

func (s publishedEpoch) LoadImmutableMarketEpoch(ctx context.Context) (fleet.ImmutableMarketEpoch, error) {
	return s.epoch, ctx.Err()
}

// Go activates, signs and persists each leg before its send, lands the
// withdrawal (whose response the RPC loses), lets a keyless owner recover it,
// then swaps through Jupiter and deposits into the USDT reserve.
func TestConnectedCrossMintExecution(t *testing.T) {
	bank := fleet.NewConnectedBank(t, fleet.ConnectedCrossMint)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	owner := "connected-cross-mint"
	revalidator, err := bank.Revalidator(owner, false)
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := fleetexec.NewRevalidatorCrossMint(revalidator)
	if err != nil {
		t.Fatal(err)
	}
	store, err := fleetexec.NewStore(ctx, bank.Pool)
	if err != nil {
		t.Fatal(err)
	}
	rpc, err := fleetexec.NewRPCAdapter(bank.RPCURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config := fleetexec.Config{Cluster: bank.Cluster, Owner: owner, LeaseTTL: 20 * time.Second, BatchSize: 1, TickInterval: 20 * time.Millisecond, SlotDuration: 400 * time.Millisecond, Facts: engine.NewFacts(prometheus.NewRegistry())}
	controller, err := fleetexec.NewCrossMintController(store, adapters, fleetexec.DelegateSigner{FeePayer: bank.Signer}, rpc, bank.Cluster, owner, config.LeaseTTL, true)
	if err != nil {
		t.Fatal(err)
	}
	var epochJSON []byte
	var epoch fleet.ImmutableMarketEpoch
	if err := bank.Pool.QueryRow(ctx, `SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1`, bank.EpochID).Scan(&epochJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(epochJSON, &epoch); err != nil {
		t.Fatal(err)
	}
	controller.SetMarketEpochSource(publishedEpoch{epoch})
	runtime, err := fleetexec.NewCrossMintRuntime(ctx, config, store, controller, rpc, adapters)
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetActivationSource(adapters)
	for phase := 0; phase < 2; phase++ {
		if n, err := runtime.Tick(ctx); err != nil || n != 1 {
			t.Fatalf("activation/signed publication phase %d n=%d: %v", phase, n, err)
		}
	}
	var withdrawal, decisionID int64
	var wire []byte
	var state string
	var broadcasts int
	if err := bank.Pool.QueryRow(ctx, `SELECT id,decision_id,signed_transaction,submission_state,broadcast_count FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1 AND movement_leg='withdraw'`, bank.OpportunityID).Scan(&withdrawal, &decisionID, &wire, &state, &broadcasts); err != nil || state != "signed" || broadcasts != 0 || bank.Sends() != 0 {
		t.Fatalf("withdrawal was not persisted before send: %s/%d %v", state, broadcasts, err)
	}
	if n, err := runtime.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("withdrawal landing n=%d: %v", n, err)
	}
	// A recovery owner with no controller, activation source or key lands the
	// executed withdrawal from its signature.
	recoveryConfig := config
	recoveryConfig.Owner = owner + "-recovery"
	recovery, err := fleetexec.NewCrossMintRecoveryRuntime(ctx, recoveryConfig, store, rpc, adapters)
	if err != nil {
		t.Fatal(err)
	}
	for state != "reconciled" {
		if _, err := recovery.Tick(ctx); err != nil {
			t.Fatalf("keyless recovery: %v", err)
		}
		if err := bank.Pool.QueryRow(ctx, `SELECT submission_state FROM loyal_yield.signed_route_submissions WHERE id=$1`, withdrawal).Scan(&state); err != nil {
			t.Fatal(err)
		}
		pause(t, ctx)
	}
	var terminal *string
	for terminal == nil {
		if _, err := runtime.Tick(ctx); err != nil {
			t.Fatalf("swap/deposit continuation: %v", err)
		}
		if err := bank.Pool.QueryRow(ctx, `SELECT terminal_outcome FROM loyal_yield.rebalance_decisions WHERE id=$1`, decisionID).Scan(&terminal); err != nil {
			t.Fatal(err)
		}
		pause(t, ctx)
	}
	var custody, version, swapCredit int64
	var status, opportunity string
	var legs, reconciled, once int
	var recovered []byte
	if err := bank.Pool.QueryRow(ctx, `SELECT d.custody_amount_raw,d.custody_version,d.status::text,o.opportunity_state,
 (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),
 (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND submission_state='reconciled'),
 (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND broadcast_count=1),
 (SELECT (reconciled_effect->'credit'->>'amountRaw')::bigint FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND movement_leg='swap'),
 (SELECT signed_transaction FROM loyal_yield.signed_route_submissions WHERE id=$2)
 FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id WHERE d.id=$1`, decisionID, withdrawal).Scan(&custody, &version, &status, &opportunity, &legs, &reconciled, &once, &swapCredit, &recovered); err != nil {
		t.Fatal(err)
	}
	if *terminal != "completed_target" || custody != 0 || version != 3 || status != "confirmed" || opportunity != "completed" || legs != 3 || reconciled != 3 || once != 3 || !bytes.Equal(wire, recovered) || bank.Sends() != 3 || bank.LostResponses() != 1 {
		t.Fatalf("terminal %s custody%d version%d %s/%s legs%d/%d/%d sends%d", *terminal, custody, version, status, opportunity, legs, reconciled, once, bank.Sends())
	}
	_, obligations, err := fleet.NewRPCClient(bank.RPCURL).FinalizedAccounts(ctx, []string{bank.Source.Obligation, bank.Target.Obligation}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if source, target := obligationCollateral(t, obligations[0], bank.Vault), obligationCollateral(t, obligations[1], bank.Vault); source != 1 || target == 0 || int64(target) != swapCredit {
		t.Fatalf("chain collateral source=%d target=%d swap credit=%d", source, target, swapCredit)
	}
}

func pause(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("Go lifecycle timed out")
	}
}

// verifySameMintWire checks the signed message executes every protected
// instruction through its own Squads policy wrapper, and for a setup route
// funds the vault and initializes the obligation under the setup policy.
func verifySameMintWire(t *testing.T, ctx context.Context, bank *fleet.ConnectedBank, wire []byte, kind fleet.ConnectedKind) {
	t.Helper()
	tx, err := sdk.TransactionFromBytes(wire)
	if err != nil || tx.VerifySignatures() != nil {
		t.Fatalf("signed wire is invalid: %v", err)
	}
	tables := map[sdk.PublicKey]sdk.PublicKeySlice{}
	rows, err := bank.Pool.Query(ctx, `SELECT t.table_address,array_agg(a.address ORDER BY a.ordinal) FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_addresses a ON a.route_lookup_table_id=t.id WHERE t.cluster=$1 GROUP BY t.id`, bank.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var address string
		var members []string
		if err := rows.Scan(&address, &members); err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			tables[sdk.MustPublicKeyFromBase58(address)] = append(tables[sdk.MustPublicKeyFromBase58(address)], sdk.MustPublicKeyFromBase58(member))
		}
	}
	rows.Close()
	if err := tx.Message.SetAddressTables(tables); err != nil {
		t.Fatal(err)
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		t.Fatal(err)
	}
	var policies []string
	transfers := 0
	for _, instruction := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(instruction.ProgramIDIndex)
		if err != nil {
			t.Fatal(err)
		}
		accounts, err := instruction.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			t.Fatal(err)
		}
		switch program.String() {
		case fleet.SquadsProgram:
			policies = append(policies, accounts[0].PublicKey.String())
		case sdk.SystemProgramID.String():
			if len(accounts) != 2 || accounts[1].PublicKey.String() != bank.Vault || binary.LittleEndian.Uint32(instruction.Data[:4]) != 2 {
				t.Fatal("system instruction is not the vault rent top-up")
			}
			transfers++
		}
	}
	route := policies[0]
	want := []string{route, route}
	if kind == fleet.ConnectedSameMintSetup {
		want = []string{route, bank.SetupPolicy, route}
		if transfers != 1 {
			t.Fatalf("setup route funded the vault %d times", transfers)
		}
	}
	if len(policies) != len(want) {
		t.Fatalf("policy executions %v, want %v", policies, want)
	}
	for i := range want {
		if policies[i] != want[i] {
			t.Fatalf("policy executions %v, want %v", policies, want)
		}
	}
}

func waitSubmission(t *testing.T, ctx context.Context, bank *fleet.ConnectedBank, worker *fleetexec.Worker, id int64, want string) string {
	t.Helper()
	var state string
	for {
		if err := worker.Tick(ctx); err != nil {
			t.Fatalf("Go tick: %v", err)
		}
		if err := bank.Pool.QueryRow(ctx, `SELECT submission_state FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == want {
			return state
		}
		if state == "failed" || state == "expired" {
			t.Fatalf("executed route became %s", state)
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("route stuck in %s waiting for %s", state, want)
		}
	}
}

// obligationCollateral reads KLend's pinned Obligation layout directly.
func obligationCollateral(t *testing.T, account fleet.Account, vault string) uint64 {
	t.Helper()
	if account.Owner != fleet.KLendProgram || len(account.Data) != 3344 || !bytes.Equal(account.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167}) || sdk.PublicKeyFromBytes(account.Data[64:96]).String() != vault {
		t.Fatalf("obligation %s identity differs", account.Address)
	}
	return binary.LittleEndian.Uint64(account.Data[128:136])
}

func connectedRPC(t *testing.T, ctx context.Context, endpoint, method string, params []any, target any) {
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
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil || len(envelope.Error) > 0 || len(envelope.Result) == 0 {
		t.Fatalf("fixture RPC %s failed", method)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatal(err)
	}
}
