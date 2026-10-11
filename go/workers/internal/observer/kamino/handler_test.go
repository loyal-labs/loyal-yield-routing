package kamino

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// Before any slot rate is measured, a Legacy-basis reserve's updates are
// skipped, logged once, and never fail the shared stream; no store is
// reached. An early-repay penalty, which no model prices, is skipped the same
// way, under its own reason.
func TestUnpriceableReserveRowsAreSkippedAndLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	market, mint, reserveKey := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	marketString, mintString := market.String(), mint.String()
	handler := NewHandler(nil, nil, slog.New(slog.NewTextHandler(&logs, nil)), false)
	handler.SetTargets([]Target{{Reserve: reserveKey.String(), Market: &marketString, LiquidityMint: &mintString}})
	update := func(slot uint64, data []byte) *pb.SubscribeUpdate {
		return &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: slot, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: reserveKey[:], Owner: klend.ProgramID.Bytes(), Data: data, WriteVersion: 1, Lamports: 1}}}}
	}
	legacy := reserveAccount(market, mint, 999, 1_000_000)
	earlyRepay := reserveAccount(market, mint, 999, 1_000_000)
	earlyRepay[4856+7] = 1
	for slot, data := range map[uint64][]byte{1_000: legacy, 1_001: legacy, 1_002: earlyRepay, 1_003: earlyRepay} {
		if outcome, err := handler.HandleAccount(context.Background(), update(slot, data)); err != nil || outcome.Inserted || outcome.Malformed {
			t.Fatalf("slot %d: outcome = %+v, err = %v; want a skipped row", slot, outcome, err)
		}
	}
	for _, reason := range []string{"reason=slot_rate_unavailable", "reason=unsupported_interest_terms"} {
		if n := strings.Count(logs.String(), reason); n != 1 {
			t.Fatalf("%s logged %d times, want once:\n%s", reason, n, logs.String())
		}
	}
}
