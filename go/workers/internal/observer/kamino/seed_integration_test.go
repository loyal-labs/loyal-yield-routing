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

// The seed and the stream are the only Kamino writers: the seed's confirmed
// read lands as an http_snapshot row and each later stream update as a
// laserstream_grpc row, both in reserve_updates. A reserve added mid-run is
// seeded alone.
func TestSeedAndStreamUpdatePersistConfirmedRows(t *testing.T) {
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
	handler := NewHandler(NewStore(pool, "kamino"), client, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	handler.SetTargets([]Target{{Reserve: reserve, Market: &marketString, LiquidityMint: &mintString}})
	handler.SetSlotRate(4.6)
	latest := func(reserve string) (source, hash string, slot int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT source,account_data_hash,slot FROM kamino.reserve_updates WHERE reserve=$1 ORDER BY slot DESC LIMIT 1`, reserve).Scan(&source, &hash, &slot); err != nil {
			t.Fatal(err)
		}
		return source, hash, slot
	}

	const seedSlot = 1_000
	seeded := reserveAccount(market, mint, seedSlot, 1_000_000)
	rpcState.set(seedSlot, seeded)
	if slot, err := handler.Seed(ctx, handler.Targets()); err != nil || slot != seedSlot {
		t.Fatalf("seed = %d, %v", slot, err)
	}
	if source, hash, slot := latest(reserve); source != "http_snapshot" || hash != accountHash(seeded) || slot != seedSlot {
		t.Fatalf("seed row = %s %s %d", source, hash, slot)
	}

	const streamSlot = seedSlot + 300
	moved := reserveAccount(market, mint, streamSlot, 900_000)
	update := &pb.SubscribeUpdate{Filters: []string{"kamino_reserves"}, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: streamSlot, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: reserveKey[:], Owner: klend.ProgramID.Bytes(), Data: moved, WriteVersion: 1, Lamports: 1}}}}
	if outcome, err := handler.HandleAccount(ctx, update); err != nil || !outcome.Inserted {
		t.Fatalf("stream update not persisted: %+v %v", outcome, err)
	}
	if source, hash, slot := latest(reserve); source != "laserstream_grpc" || hash != accountHash(moved) || slot != streamSlot {
		t.Fatalf("stream row = %s %s %d", source, hash, slot)
	}

	addedKey := solana.NewWallet().PublicKey()
	added := addedKey.String()
	addedState := reserveAccount(market, mint, streamSlot+10, 500_000)
	rpcState.set(streamSlot+10, addedState)
	if _, err := handler.Seed(ctx, []Target{{Reserve: added, Market: &marketString, LiquidityMint: &mintString}}); err != nil {
		t.Fatal(err)
	}
	if source, hash, slot := latest(added); source != "http_snapshot" || hash != accountHash(addedState) || slot != streamSlot+10 {
		t.Fatalf("added reserve row = %s %s %d", source, hash, slot)
	}
	if source, _, slot := latest(reserve); source != "laserstream_grpc" || slot != streamSlot {
		t.Fatalf("seeding an added reserve rewrote a watched one: %s %d", source, slot)
	}
}
