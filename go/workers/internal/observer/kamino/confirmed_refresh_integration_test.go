package kamino

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// confirmedAccountRPC serves getMultipleAccounts for one reserve from a
// mutable confirmed state, as a Solana RPC node would.
type confirmedAccountRPC struct {
	mu   sync.Mutex
	slot uint64
	data []byte
}

func (c *confirmedAccountRPC) set(slot uint64, data []byte) {
	c.mu.Lock()
	c.slot, c.data = slot, data
	c.mu.Unlock()
}

func (c *confirmedAccountRPC) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	var body struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Method != "getMultipleAccounts" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	slot, data := c.slot, c.data
	c.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": map[string]any{
		"context": map[string]any{"slot": slot},
		"value": []any{map[string]any{"lamports": 1, "owner": klend.ProgramID.String(), "executable": false, "rentEpoch": 0,
			"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}},
	}})
}

// reserveAccount is a KLend reserve whose last refresh happened at
// lastUpdateSlot with the given available liquidity.
func reserveAccount(market, mint solana.PublicKey, lastUpdateSlot, available uint64) []byte {
	data := make([]byte, klend.ReserveSize)
	copy(data, klend.ReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 1)
	body := data[8:]
	copy(body[24:56], market[:])
	binary.LittleEndian.PutUint64(body[8:16], lastUpdateSlot)
	liquidity := data[128:1360]
	copy(liquidity[:32], mint[:])
	binary.LittleEndian.PutUint64(liquidity[96:104], available)
	putFraction(liquidity[104:120], 50)
	putFraction(liquidity[120:136], 1)
	binary.LittleEndian.PutUint64(liquidity[144:152], 6)
	for index := range 4 {
		binary.LittleEndian.PutUint64(liquidity[168+index*8:], uint64(index+1))
	}
	return data
}

type verifiedRow struct {
	hash                                string
	stateSlot, verifiedSlot, lastUpdate int64
	verifiedAt                          time.Time
	verificationSource                  string
	present                             bool
}

func latestVerified(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reserve string) verifiedRow {
	t.Helper()
	var row verifiedRow
	err := pool.QueryRow(ctx, `SELECT account_data_hash,slot,verified_slot,reserve_last_update_slot,verified_at,verification_source FROM kamino.latest_verified_reserve_updates WHERE reserve=$1`, reserve).
		Scan(&row.hash, &row.stateSlot, &row.verifiedSlot, &row.lastUpdate, &row.verifiedAt, &row.verificationSource)
	if err == nil {
		row.present = true
	}
	return row
}

// Production 2026-10-08 01:46-01:52: the Go observer persisted every stream
// update but confirmed it only on its periodic pass, so the stream floor
// evicted active reserves from latest_verified_reserve_updates for up to a
// minute (median 30 s from stream row to confirmed row, against 80 ms in
// Rust) and the planner's mints went incomplete. Rust marks each durable
// stream write dirty and confirms it on the next 100 ms batch; the periodic
// timer is only a safety sweep that keeps quiet reserves' verified_at fresh.
func TestStreamUpdateIsConfirmedOnNextBatchAndQuietReserveStaysVerified(t *testing.T) {
	databaseURL := os.Getenv("TEST_TIMESCALE_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_TIMESCALE_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	market, mint, reserveKey := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	marketString, mintString, reserve := market.String(), mint.String(), reserveKey.String()
	rpcState := &confirmedAccountRPC{}
	server := httptest.NewServer(rpcState)
	defer server.Close()
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewStore(pool, "kamino"), client, slog.New(slog.NewTextHandler(io.Discard, nil)), 400, false)
	handler.SetTargets([]Target{{Reserve: reserve, Market: &marketString, LiquidityMint: &mintString}})

	const seedSlot = 1_000
	seeded := reserveAccount(market, mint, seedSlot, 1_000_000)
	rpcState.set(seedSlot, seeded)
	if _, err := handler.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if row := latestVerified(ctx, t, pool, reserve); !row.present || row.hash != accountHash(seeded) {
		t.Fatalf("seeded reserve is not verified: %+v", row)
	}

	// A borrow refreshes the reserve on chain. The stream sees it first, far
	// enough past the verified slot that the floor evicts the old proof.
	const streamSlot = seedSlot + 300
	moved := reserveAccount(market, mint, streamSlot, 900_000)
	update := &pb.SubscribeUpdate{Filters: []string{"kamino_reserves"}, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: streamSlot, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: reserveKey[:], Owner: klend.ProgramID.Bytes(), Data: moved, WriteVersion: 1, Lamports: 1}}}}
	if outcome, err := handler.HandleAccount(ctx, update); err != nil || !outcome.Inserted {
		t.Fatalf("stream update not persisted: %+v %v", outcome, err)
	}
	if row := latestVerified(ctx, t, pool, reserve); row.present {
		t.Fatalf("stale proof survived a newer stream state: %+v", row)
	}

	// The next batch tick, not the safety sweep, confirms the stream state.
	rpcState.set(streamSlot+1, moved)
	ran, err := handler.VerifyDirty(ctx)
	if err != nil || !ran {
		t.Fatalf("stream update was not queued for a confirmed read: ran=%v err=%v", ran, err)
	}
	confirmed := latestVerified(ctx, t, pool, reserve)
	if !confirmed.present || confirmed.hash != accountHash(moved) || confirmed.stateSlot != streamSlot+1 || confirmed.verifiedSlot != streamSlot+1 || confirmed.lastUpdate != streamSlot || confirmed.verificationSource != "http_confirmed_refresh" {
		t.Fatalf("planner view after one batch = %+v, want the stream state confirmed at slot %d", confirmed, streamSlot+1)
	}
	if ran, err := handler.VerifyDirty(ctx); err != nil || ran {
		t.Fatalf("confirmed reserve stayed queued: ran=%v err=%v", ran, err)
	}

	// Quiet reserve: the account does not change, so Rust writes no new
	// reserve_updates row, but the sweep must still advance verified_at and
	// verified_slot, which bound the planner's 240 s verification age.
	var rowsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kamino.reserve_updates WHERE reserve=$1`, reserve).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	rpcState.set(streamSlot+600, moved)
	handler.RequestSafetySweep()
	if ran, err := handler.VerifyDirty(ctx); err != nil || !ran {
		t.Fatalf("safety sweep did not run: ran=%v err=%v", ran, err)
	}
	swept := latestVerified(ctx, t, pool, reserve)
	if !swept.present || swept.hash != confirmed.hash || swept.verifiedSlot != streamSlot+600 || !swept.verifiedAt.After(confirmed.verifiedAt) {
		t.Fatalf("quiet reserve proof was not renewed: before=%+v after=%+v", confirmed, swept)
	}
	var rowsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kamino.reserve_updates WHERE reserve=$1`, reserve).Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Fatalf("unchanged account wrote %d reserve_updates rows; Rust reuses the matched state", rowsAfter-rowsBefore)
	}
}
