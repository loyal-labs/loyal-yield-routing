package earn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// memoUpdate is a Squads transaction on settings whose memo lists the vault.
func memoUpdate(slot uint64, settings solana.PublicKey, memo string) *pb.SubscribeUpdate {
	keys := [][]byte{solana.NewWallet().PublicKey().Bytes(), settings.Bytes(), squadsVault(settings, 0).Bytes(), squads.ProgramID.Bytes(), memoProgram.Bytes()}
	return streamTransaction(slot, keys, &pb.CompiledInstruction{ProgramIdIndex: 3, Accounts: []byte{1}, Data: []byte{0}},
		&pb.CompiledInstruction{ProgramIdIndex: 4, Accounts: []byte{2}, Data: []byte(memo)})
}

func intentRows(t *testing.T, pool *pgxpool.Pool, settings solana.PublicKey) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT kind || ':' || request_id || ':' || COALESCE(amount_raw::text, 'max') || ':' || status
        FROM loyal_yield.earn_max_intents WHERE settings = $1 ORDER BY slot, signature`, settings.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func policyCursor(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var cursor int64
	if err := pool.QueryRow(context.Background(), `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name = $1`, PolicyProjectionConsumer).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	return cursor
}

// Anyone with a smart account can write a valid Earn MAX memo: one for
// settings without a route is skipped, the cursor advances, and the next
// transaction projects.
func TestForeignEarnMaxMemoNeverStallsTheStream(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	app := streamApplication(t, pool)
	foreign := solana.NewWallet().PublicKey()
	destination := solana.NewWallet().PublicKey().String()
	if err := app.HandlePolicyTransaction(ctx, memoUpdate(streamSlot+10, foreign, "loyal:earn-max:v2:withdraw:foreign-request:5:"+destination)); err != nil {
		t.Fatalf("a foreign memo stopped the stream: %v", err)
	}
	if rows := intentRows(t, pool, foreign); len(rows) != 0 {
		t.Fatalf("foreign intent recorded: %v", rows)
	}
	if cursor := policyCursor(t, pool); cursor < streamSlot+10 {
		t.Fatalf("cursor %d did not pass the foreign memo", cursor)
	}
	ours := solana.NewWallet().PublicKey()
	seedEarnMaxRoute(t, pool, ours, 0)
	if err := app.HandlePolicyTransaction(ctx, memoUpdate(streamSlot+11, ours, "loyal:earn-max:v2:withdraw:our-request-1:max:"+destination)); err != nil {
		t.Fatal(err)
	}
	if rows := intentRows(t, pool, ours); len(rows) != 1 || rows[0] != "withdraw:our-request-1:max:pending" {
		t.Fatalf("our intent %v", rows)
	}
}

func seedEarnMaxRoute(t *testing.T, pool *pgxpool.Pool, settings solana.PublicKey, claimRaw uint64) *multiply.RouteState {
	t.Helper()
	ctx := context.Background()
	routeKey := "earn-max:" + settings.String() + ":0"
	clean := func() {
		for _, statement := range []string{
			`DELETE FROM loyal_yield.multiply_position_snapshots WHERE route_key = $1`,
			`DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`,
			`DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`,
			`DELETE FROM loyal_yield.earn_max_intents WHERE settings = split_part($1, ':', 2)`,
		} {
			if _, err := pool.Exec(ctx, statement, routeKey); err != nil {
				t.Fatal(err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	topology, err := multiply.DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	route, err := multiply.NewRouteState(routeKey, settings.String(), 0, topology.Vault.String(), 320,
		multiply.TokenBalance{Account: topology.ClaimCustody.String(), Mint: multiply.USDCMint, TokenProgram: multiply.TokenProgram, AmountRaw: claimRaw}, 900, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store, err := multiply.NewStoreFromPool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.CreateRouteState(ctx, route); err != nil || !created {
		t.Fatalf("route %v %v", created, err)
	}
	return route
}

// Our withdraw memo is recorded once however often the stream replays it, a
// second withdraw is recorded without stalling the stream, the multiply
// worker's decision applies the first, and the root claim of that request
// reconciles.
func TestEarnMaxWithdrawMemoIsAppliedAndItsClaimReconciles(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../../multiply/testdata/svm-root-wallet-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Settings, Source, Destination, Signature, RequestID, WireBase64                                      string
		AmountRaw, ConfirmedSlot, SourceBeforeRaw, SourceAfterRaw, DestinationBeforeRaw, DestinationAfterRaw uint64
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	settings := solana.MustPublicKeyFromBase58(fixture.Settings)
	route := seedEarnMaxRoute(t, pool, settings, fixture.SourceBeforeRaw)
	app := streamApplication(t, pool)
	withdraw := memoUpdate(950, settings, "loyal:earn-max:v2:withdraw:"+fixture.RequestID+":100000:"+fixture.Destination)
	for range 2 {
		if err := app.HandlePolicyTransaction(ctx, withdraw); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.HandlePolicyTransaction(ctx, memoUpdate(951, settings, "loyal:earn-max:v2:withdraw:second-request:5:"+fixture.Destination)); err != nil {
		t.Fatalf("a second withdrawal stopped the stream: %v", err)
	}
	if rows := intentRows(t, pool, settings); len(rows) != 2 || rows[0] != "withdraw:"+fixture.RequestID+":100000:pending" || rows[1] != "withdraw:second-request:5:pending" {
		t.Fatalf("recorded intents %v", rows)
	}

	// The multiply worker's decision under its lease.
	store, err := multiply.NewStoreFromPool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.LeaseRoute(ctx, route.RouteKey, "multiply-worker", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease %v %v", lease, err)
	}
	if moved, err := store.ApplyPendingIntents(ctx, lease, route, "1000000"); err != nil || !moved {
		t.Fatalf("apply %v %v", moved, err)
	}
	if rows := intentRows(t, pool, settings); rows[0] != "withdraw:"+fixture.RequestID+":100000:applied" || rows[1] != "withdraw:second-request:5:refused" {
		t.Fatalf("decided intents %v", rows)
	}
	// The worker's unwind makes the request claimable.
	saved, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	next := *saved.State
	withdrawal := *next.Withdrawal
	withdrawal.Status = multiply.WithdrawalClaimable
	next.Withdrawal, next.Generation = &withdrawal, next.Generation+1
	if ok, err := store.SaveRouteState(ctx, lease, &next); err != nil || !ok {
		t.Fatalf("claimable %v %v", ok, err)
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}

	wire, err := base64.StdEncoding.DecodeString(fixture.WireBase64)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := app.projectCashFlow(ctx, settings, fixture.RequestID, &custodyTransfer{wire: wire, signature: fixture.Signature, slot: fixture.ConfirmedSlot,
		source: solana.MustPublicKeyFromBase58(fixture.Source), destination: solana.MustPublicKeyFromBase58(fixture.Destination),
		sourcePre: fixture.SourceBeforeRaw, sourcePost: fixture.SourceAfterRaw, destinationPre: fixture.DestinationBeforeRaw, destinationPost: fixture.DestinationAfterRaw})
	if err != nil || !claimed {
		t.Fatalf("claim %v %v", claimed, err)
	}
	final, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil || final.State.Withdrawal.Status != multiply.WithdrawalClaimed || *final.State.Withdrawal.ClaimSignature != fixture.Signature {
		t.Fatalf("claim not reconciled %v", err)
	}
}
