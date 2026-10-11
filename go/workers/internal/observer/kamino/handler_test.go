package kamino

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// A Legacy-basis reserve update arriving while the slot rate cannot be read
// is an error the stream retries, never a row skipped as malformed or a rate
// stored at a guessed slot rate. No store is reached.
func TestStreamUpdateWithoutAMeasuredSlotRateIsRetried(t *testing.T) {
	server := httptest.NewServer(&confirmedAccountRPC{})
	defer server.Close()
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	market, mint, reserveKey := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	marketString, mintString := market.String(), mint.String()
	handler := NewHandler(nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	handler.SetTargets([]Target{{Reserve: reserveKey.String(), Market: &marketString, LiquidityMint: &mintString}})
	update := &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: 1_000, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: reserveKey[:], Owner: klend.ProgramID.Bytes(), Data: reserveAccount(market, mint, 999, 1_000_000), WriteVersion: 1, Lamports: 1}}}}
	outcome, err := handler.HandleAccount(context.Background(), update)
	if !errors.Is(err, klend.ErrSlotRateUnavailable) || outcome.Malformed {
		t.Fatalf("outcome = %+v, err = %v; want a retried ErrSlotRateUnavailable", outcome, err)
	}
}
