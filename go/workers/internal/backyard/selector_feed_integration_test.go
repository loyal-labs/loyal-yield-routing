package backyard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// The selector feed reads each reserve's latest confirmed stream row by slot,
// with that row's own observed time and slot: an hour-old row of a quiet
// reserve stays its latest, an older slot replayed later never replaces it, and
// a retired verifier row is not the stream's.
func TestReserveFeedReadsLatestStreamRowBySlot(t *testing.T) {
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
	store := kamino.NewStore(pool, "kamino")
	market, mint := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	collateral, debt := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	now := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(reserve, source string, slot uint64, hash string, observedAt time.Time) {
		t.Helper()
		snapshot := kamino.Snapshot{ObservationSchemaVersion: 2, ObservedAt: observedAt, Slot: slot, Reserve: reserve, Market: &market, LiquidityMint: mint, MintDecimals: 6, BorrowedAmountSF: "0"}
		if _, err := store.Insert(ctx, kamino.Record{Target: kamino.Target{Reserve: reserve, Market: &market, LiquidityMint: &mint}, Snapshot: snapshot, DiffSummary: "fixture", Source: source, SourceCommitment: "confirmed", AccountHash: hash, ReceivedAt: observedAt, DecodedAt: observedAt}); err != nil {
			t.Fatal(err)
		}
	}
	insert(collateral, "http_snapshot", 2_000, strings.Repeat("a", 64), now.Add(-time.Hour))
	insert(collateral, "laserstream_grpc", 1_900, strings.Repeat("b", 64), now)
	insert(collateral, "http_confirmed_refresh", 2_100, strings.Repeat("c", 64), now)
	insert(debt, "laserstream_grpc", 2_050, strings.Repeat("d", 64), now)

	rows, err := readReserveEconomics(ctx, pool, []RuntimeRoute{{Kamino: KaminoObservationConfig{CollateralReserve: collateral, DebtReserve: debt}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[collateral]; got.Slot != 2_000 || got.Hash != strings.Repeat("a", 64) || !got.ObservedAt.Equal(now.Add(-time.Hour)) || got.Commitment != "confirmed" || got.Market != market || got.Mint != mint {
		t.Fatalf("collateral row = %+v, want the seed at slot 2000 observed an hour ago", got)
	}
	if got := rows[debt]; got.Slot != 2_050 || !got.ObservedAt.Equal(now) {
		t.Fatalf("debt row = %+v, want the stream row at slot 2050", got)
	}
}
