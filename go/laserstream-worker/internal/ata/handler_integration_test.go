package ata

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

func TestHandlerPersistsRealATAObservationSchema(t *testing.T) {
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
	ataKey, tokenOwner := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	mint := solana.MustPublicKeyFromBase58(usdcMint)
	data := make([]byte, 165)
	copy(data[:32], mint[:])
	copy(data[32:64], tokenOwner[:])
	binary.LittleEndian.PutUint64(data[64:72], 123)
	handler := NewHandler(pool, "production", nil)
	handler.SetTargets(map[string]watch.ATATarget{ataKey.String(): {ID: 1, Cluster: "mainnet", Wallet: tokenOwner.String(), WalletATA: ataKey.String(), Vault: solana.NewWallet().PublicKey().String(), VaultATA: solana.NewWallet().PublicKey().String(), Mint: mint.String()}})
	update := &pb.SubscribeUpdate{Filters: []string{watch.BalanceSweepWalletATAs}, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: 50, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: ataKey[:], Lamports: 2_039_280, Owner: solana.TokenProgramID[:], Data: data, TxnSignature: []byte{1, 2, 3}}}}}
	first, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted {
		t.Fatal("first ATA observation was not inserted")
	}
	duplicate, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Inserted || duplicate.EventID != first.EventID {
		t.Fatalf("ATA duplicate = %+v, first = %+v", duplicate, first)
	}
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT amount_raw FROM loyal_prod.balance_sweep_wallet_ata_observations WHERE event_id=$1`, first.EventID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != 123 {
		t.Fatalf("persisted ATA amount = %d", amount)
	}
}
