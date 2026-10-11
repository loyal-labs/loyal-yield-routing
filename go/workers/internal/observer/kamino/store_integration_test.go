package kamino

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

func TestStorePersistsAndDeduplicatesAgainstRealSchema(t *testing.T) {
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
	store := NewStore(pool, "kamino")
	reserve := "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
	mint := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	market := "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	observed := time.Now().UTC()
	snapshot := Snapshot{ObservationSchemaVersion: 2, ObservedAt: observed, Slot: 100, Reserve: reserve, Market: &market, LiquidityMint: mint, MintDecimals: 6, BorrowedAmountSF: "0"}
	target := Target{Reserve: reserve, Market: &market, LiquidityMint: &mint}
	record := Record{Target: target, Snapshot: snapshot, DiffSummary: "fixture", Source: "laserstream_grpc", SourceCommitment: "confirmed", AccountHash: "stream-hash", ReceivedAt: observed, DecodedAt: observed}
	stream, err := store.Insert(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Inserted {
		t.Fatal("first stream record was not inserted")
	}
	duplicate, err := store.Insert(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Inserted {
		t.Fatalf("stream duplicate = %+v, first = %+v", duplicate, stream)
	}
}

// A reserve update whose decoded fields did not change is still persisted,
// with an empty changed_fields array (Rust binds an empty Vec). A nil list
// reached the NOT NULL column as NULL and failed the whole stream session.
func TestStorePersistsUnchangedReserveUpdateAgainstRealSchema(t *testing.T) {
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
	reserve := "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX"
	mint := "GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm"
	market := "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk"
	observed := time.Now().UTC()
	slot := uint64(observed.UnixNano())
	previous := Snapshot{ObservationSchemaVersion: 2, ObservedAt: observed, Slot: slot - 1, Reserve: reserve, Market: &market, LiquidityMint: mint, MintDecimals: 6, BorrowedAmountSF: "0"}
	current := previous
	current.Slot = slot
	diff := Compare(previous, current)
	record := Record{Target: Target{Reserve: reserve, Market: &market, LiquidityMint: &mint}, Snapshot: current, Diff: &diff, DiffSummary: "unchanged", Source: "laserstream_grpc", SourceCommitment: "confirmed", AccountHash: "unchanged-" + strconv.FormatUint(slot, 10), ReceivedAt: observed, DecodedAt: observed}
	outcome, err := NewStore(pool, "kamino").Insert(ctx, record)
	if err != nil {
		t.Fatalf("unchanged reserve update was not persisted: %v", err)
	}
	var fields []string
	var diffFields string
	if err := pool.QueryRow(ctx, `SELECT changed_fields, diff->'changed_fields' FROM kamino.reserve_updates WHERE event_id=$1`, outcome.EventID).Scan(&fields, &diffFields); err != nil {
		t.Fatal(err)
	}
	if fields == nil || len(fields) != 0 || diffFields != "[]" {
		t.Fatalf("unchanged update stored changed_fields=%v diff=%s, want {} and []", fields, diffFields)
	}
}

// reserve_current_states names each reserve's newest stored row: a later
// slot replaces it, a replayed older slot or a duplicate never does, and a
// stream row may hold it (0011).
func TestStoreAdvancesCurrentStateToTheNewestSlot(t *testing.T) {
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
	store := NewStore(pool, "kamino")
	reserve := solana.NewWallet().PublicKey().String()
	market, mint := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	insert := func(source string, slot uint64) InsertOutcome {
		t.Helper()
		observed := time.Now().UTC()
		snapshot := Snapshot{ObservationSchemaVersion: 2, ObservedAt: observed, Slot: slot, Reserve: reserve, Market: &market, LiquidityMint: mint, MintDecimals: 6, BorrowedAmountSF: "0"}
		outcome, err := store.Insert(ctx, Record{Target: Target{Reserve: reserve, Market: &market, LiquidityMint: &mint}, Snapshot: snapshot, DiffSummary: "fixture", Source: source, SourceCommitment: "confirmed", AccountHash: strconv.FormatUint(slot, 10), ReceivedAt: observed, DecodedAt: observed})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	current := func() (eventID, slot int64, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT state_event_id,state_slot,state_source FROM kamino.reserve_current_states WHERE reserve=$1`, reserve).Scan(&eventID, &slot, &source); err != nil {
			t.Fatal(err)
		}
		return eventID, slot, source
	}
	seed := insert("http_snapshot", 2_000)
	insert("laserstream_grpc", 1_900) // replayed after the seed
	if id, slot, source := current(); id != seed.EventID || slot != 2_000 || source != "http_snapshot" {
		t.Fatalf("an older replayed slot moved the current state: %d %d %s", id, slot, source)
	}
	newer := insert("laserstream_grpc", 2_100)
	insert("laserstream_grpc", 2_100) // a duplicate
	if id, slot, source := current(); id != newer.EventID || slot != 2_100 || source != "laserstream_grpc" {
		t.Fatalf("the newest stream row is not current: %d %d %s", id, slot, source)
	}
}
