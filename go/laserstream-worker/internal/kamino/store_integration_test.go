package kamino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStorePersistsAndVerifiesAgainstRealSchema(t *testing.T) {
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
	if duplicate.Inserted || duplicate.EventID != stream.EventID {
		t.Fatalf("stream duplicate = %+v, first = %+v", duplicate, stream)
	}
	record.Source = "http_confirmed_refresh"
	verification := Verification{Reserve: reserve, AccountHash: record.AccountHash, VerifiedSlot: 100, VerifiedAt: observed, Commitment: "confirmed", Source: record.Source, StateValid: true}
	classified, err := store.VerifyStates(ctx, []Verification{verification})
	if err != nil {
		t.Fatal(err)
	}
	if len(classified.Matched) != 0 || len(classified.Deferred) != 0 {
		t.Fatalf("first HTTP state was prematurely classified: %+v", classified)
	}
	http, err := store.Insert(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if !http.Inserted || !http.CurrentStateAdmitted || !http.VerificationAdmitted {
		t.Fatalf("confirmed HTTP record not fully admitted: %+v", http)
	}
	classified, err = store.VerifyStates(ctx, []Verification{verification})
	if err != nil {
		t.Fatal(err)
	}
	if _, matched := classified.Matched[reserve]; !matched {
		t.Fatalf("persisted HTTP state did not match on exact reread: %+v", classified)
	}
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kamino.latest_verified_reserve_updates WHERE reserve=$1)`, reserve).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("admitted reserve is absent from latest_verified_reserve_updates")
	}
	if err := store.RecordMalformed(ctx, reserve, 200, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	classified, err = store.VerifyStates(ctx, []Verification{verification})
	if err != nil {
		t.Fatal(err)
	}
	if _, deferred := classified.Deferred[reserve]; !deferred {
		t.Fatalf("stale HTTP proof crossed invalid stream floor: %+v", classified)
	}
}
