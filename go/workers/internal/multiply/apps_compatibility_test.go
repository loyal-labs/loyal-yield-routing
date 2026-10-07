package multiply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Execute the retained Rust producer's actual SQL, not a proposed native Go
// admission API. This proves registered-schema/Go-reader interoperability;
// external receipt authority remains in the expressly retained Rust bridge.
func retainedExternalStatements(t *testing.T) (string, string, string) {
	t.Helper()
	raw, err := os.ReadFile("../../../../crates/loyal-yield-store/src/multiply_state_store.rs")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	begin := strings.Index(source, "pub async fn admit_external_multiply_operation(")
	end := strings.Index(source[begin:], "pub async fn persist_signed_operation(") + begin
	block := source[begin:end]
	insert := regexp.MustCompile(`"(INSERT INTO loyal_yield.multiply_operations [^"\n]+)"`).FindStringSubmatch(block)
	snapshot := regexp.MustCompile(`(?s)(INSERT INTO loyal_yield.multiply_position_snapshots \(.*?ON CONFLICT \(route_key, observed_slot\) DO NOTHING)`).FindStringSubmatch(block)
	updateBlock := source[strings.Index(source, "async fn update_route_in_transaction("):]
	update := regexp.MustCompile(`"(UPDATE loyal_yield.multiply_route_states [^"\n]+)"`).FindStringSubmatch(updateBlock)
	if len(insert) != 2 || len(snapshot) != 2 || len(update) != 2 {
		t.Fatal("retained external producer statement could not be loaded")
	}
	return insert[1], update[1], snapshot[1]
}

func admitRetainedExternalFixture(ctx context.Context, store *Store, lease *Lease, route *RouteState, op *MultiplyOperation, insert, update, snapshot string) (bool, error) {
	// This mirrors the source admission guard; it is test-local and cannot create
	// a production second owner or fabricate signed worker bytes.
	if op.Status != StatusReconciled || (op.Action != ActionClaim && op.Action != ActionDepositClaimAsset) || route.CurrentOperationID != nil || op.RouteKey != lease.RouteKey || op.Cycle != route.Cycle {
		return false, errors.New("external source ownership rejected")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, insert, op.OperationID, op.RouteKey, int64(op.Cycle), EngineVersion, string(op.Action), strategyKeyText(op.StrategyKey), op.IDempotencyKey, op.ExpectedEffects, op.MessageSHA256, op.SignedWireSHA256, op.TransactionSignature, op.SourceInstructionIndex, op.RecentBlockhash, int64(*op.ConfirmedSlot), op.ReconciliationSHA256, op.CreatedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	var version int64
	err = tx.QueryRow(ctx, update, lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken, encoded).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if op.Action == ActionClaim {
		if route.Position.Claim == nil {
			return false, errors.New("source claimed route omitted idle custody")
		}
		tag, err = tx.Exec(ctx, snapshot, route.RouteKey, int64(route.Generation), int64(*op.ConfirmedSlot), route.ObservedAt, route.Position.Claim.AmountRaw, time.Now().UTC())
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			return false, errors.New("claim source snapshot already exists")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

func TestRetainedExternalClaimBareJournalIsAtomicAndReadable(t *testing.T) {
	store := integrationStore(t)
	insert, update, snapshot := retainedExternalStatements(t)
	raw, err := os.ReadFile("testdata/svm-root-wallet-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Settings, Vault, Source, Destination, Signature, RequestID                                           string
		VaultIndex                                                                                           uint8
		AmountRaw, ConfirmedSlot, SourceBeforeRaw, SourceAfterRaw, DestinationBeforeRaw, DestinationAfterRaw uint64
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key := "earn-max:" + fixture.Settings + ":0"
	state, err := NewRouteState(key, fixture.Settings, fixture.VaultIndex, fixture.Vault, 320, TokenBalance{Account: fixture.Source, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: fixture.SourceBeforeRaw}, fixture.ConfirmedSlot-1, now)
	if err != nil {
		t.Fatal(err)
	}
	state.Goal = GoalWithdraw
	state.Withdrawal = &Withdrawal{RequestID: fixture.RequestID, DestinationAccount: fixture.Destination, AmountRaw: fixture.AmountRaw, Status: WithdrawalClaimable, RequestedAt: now.Add(-time.Minute), ReadyBy: now.Add(9 * time.Minute), UnwindCompletedAt: &now}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if _, err := store.pool.Exec(context.Background(), `WITH operations AS (DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1) DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key); err != nil {
			t.Error(err)
		}
	})
	if ok, err := store.CreateRouteState(ctx, state); err != nil || !ok {
		t.Fatalf("create source claimable route %v %v", ok, err)
	}
	lease, err := store.LeaseRoute(ctx, key, "retained-source-fixture", now.Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease %v %v", lease, err)
	}
	next := *state
	withdrawal := *state.Withdrawal
	next.Withdrawal = &withdrawal
	next.Generation++
	next.Goal = GoalDeploy
	next.ObservedSlot = fixture.ConfirmedSlot
	next.Withdrawal.Status = WithdrawalClaimed
	next.Withdrawal.ClaimSignature = &fixture.Signature
	next.Position = NewIdlePosition(TokenBalance{Account: fixture.Source, Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: fixture.SourceAfterRaw})
	digest := sha256.Sum256([]byte(fixture.Signature + ":account"))
	reconciled := hex.EncodeToString(digest[:])
	op := &MultiplyOperation{OperationID: "cash-" + reconciled[:32], RouteKey: key, Cycle: next.Cycle, EngineVersion: EngineVersion, Action: ActionClaim, Status: StatusReconciled, IDempotencyKey: EngineVersion + ":cash-flow:" + fixture.Signature, TransactionSignature: &fixture.Signature, ConfirmedSlot: &fixture.ConfirmedSlot, ReconciliationSHA256: &reconciled, CreatedAt: now, UpdatedAt: now, ExpectedEffects: ExpectedEffects{TokenAmountsBefore: []TokenAmountBefore{{Account: fixture.Source, Mint: USDCMint, AmountRaw: fixture.SourceBeforeRaw}, {Account: fixture.Destination, Mint: USDCMint, AmountRaw: fixture.DestinationBeforeRaw}}, TokenDeltas: []TokenDelta{{Account: fixture.Source, Mint: USDCMint, RawDelta: -int64(fixture.AmountRaw)}, {Account: fixture.Destination, Mint: USDCMint, RawDelta: int64(fixture.AmountRaw)}}}}
	stale := *lease
	stale.Version++
	if ok, err := admitRetainedExternalFixture(ctx, store, &stale, &next, op, insert, update, snapshot); err != nil || ok {
		t.Fatalf("stale source admission %v %v", ok, err)
	}
	if saved, err := store.LoadOperation(ctx, op.OperationID); err != nil || saved != nil {
		t.Fatalf("rolled-back journal %v %v", saved, err)
	}
	before, err := store.LoadRouteState(ctx, key)
	if err != nil || before.State.Withdrawal.Status != WithdrawalClaimable || before.State.Generation != state.Generation {
		t.Fatalf("stale admission changed route %v %v", before, err)
	}
	if ok, err := admitRetainedExternalFixture(ctx, store, lease, &next, op, insert, update, snapshot); err != nil || !ok {
		t.Fatalf("source reconciled admission %v %v", ok, err)
	}
	if ok, err := admitRetainedExternalFixture(ctx, store, lease, &next, op, insert, update, snapshot); err != nil || ok {
		t.Fatalf("duplicate external receipt %v %v", ok, err)
	}
	saved, err := store.LoadOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != StatusReconciled || saved.Action != ActionClaim || saved.SignedWire != nil || saved.SignedWireSHA256 != nil || saved.RecentBlockhash != nil || saved.LastValidBlockHeight != nil {
		t.Fatal("source external receipt fabricated worker signing identity")
	}
	route, err := store.LoadRouteState(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if route.Operation != nil || route.State.CurrentOperationID != nil || route.State.Withdrawal.Status != WithdrawalClaimed || route.State.Withdrawal.AmountRaw != fixture.AmountRaw || route.State.Withdrawal.DestinationAccount != fixture.Destination {
		t.Fatal("external admission lost saved request or exposed worker recovery")
	}
	var count int
	var claim string
	if err := store.pool.QueryRow(ctx, `SELECT count(*),max(claim_raw)::text FROM loyal_yield.multiply_position_snapshots WHERE route_key=$1 AND observed_slot=$2`, key, int64(fixture.ConfirmedSlot)).Scan(&count, &claim); err != nil || count != 1 || claim != "900000" {
		t.Fatalf("atomic terminal snapshot %d %s %v", count, claim, err)
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	executor, rpc, _ := testExecutor(t)
	worker, err := NewWorker(WorkerDeps{Store: store, Observer: forbiddenObservationReader{}, Executor: executor, Quotes: fakeQuoteClient{}, WorkerID: "go-after-source", Chain: surfaceChain{executor.RPC}, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.Tick(ctx)
	if err != nil || result.Condition != "no_route_available" || len(rpc.sent) != 0 {
		t.Fatalf("external terminal row became autonomous recovery %+v %v sends=%d", result, err, len(rpc.sent))
	}
}
