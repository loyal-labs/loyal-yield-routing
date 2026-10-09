package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// feeOnlyPrepared compiles a v0 message the way the revalidator does for a
// fee-only payer: the payer first and writable, the delegate a read-only signer.
func feeOnlyPrepared(t *testing.T, payer, delegate ed25519.PrivateKey) fleet.PreparedTransaction {
	t.Helper()
	ix := sdk.NewInstruction(sdk.SystemProgramID, sdk.AccountMetaSlice{sdk.Meta(sdk.PublicKeyFromBytes(delegate[32:])).SIGNER()}, []byte{9})
	tx, err := sdk.NewTransaction([]sdk.Instruction{ix}, sdk.Hash{7}, sdk.TransactionPayer(sdk.PublicKeyFromBytes(payer[32:])))
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.SetVersion(sdk.MessageVersionV0)
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	mh, wh := sha256.Sum256(message), sha256.Sum256(wire)
	return fleet.PreparedTransaction{Message: message, UnsignedWire: wire, MessageSHA256: hex.EncodeToString(mh[:]), WireSHA256: hex.EncodeToString(wh[:]),
		PacketBytes: len(wire), WritableAccounts: []string{sdk.PublicKeyFromBytes(payer[32:]).String()}, FeeLamports: 10_000, ComputeLimit: 200_000}
}

func TestFeeOnlyPayerSignsFirstAndDelegateSecond(t *testing.T) {
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	payer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	prepared := feeOnlyPrepared(t, payer, delegate)
	if _, err := (DelegateSigner{FeePayer: delegate}).SignPreparedRoute(prepared, 900); err == nil {
		t.Fatal("a fee-only payer the signer does not hold was accepted")
	}
	wire, err := (DelegateSigner{FeePayer: delegate, FeeOnly: []ed25519.PrivateKey{payer}}).SignPreparedRoute(prepared, 900)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil || len(tx.Signatures) != 2 || tx.VerifySignatures() != nil {
		t.Fatalf("two-signer wire invalid: %v", err)
	}
	if tx.Signatures[0].String() != wire.TransactionSignature || !ed25519.Verify(ed25519.PublicKey(payer[32:]), prepared.Message, tx.Signatures[0][:]) {
		t.Fatal("the transaction identity must be the fee payer's signature")
	}
}

func TestFreshFeeOnlyRoutePersistsRustSpendReservation(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := t.Context()
	a, signer := seedFresh(t, ctx, pool)
	payer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32))
	payerKey, delegateKey := sdk.PublicKeyFromBytes(payer[32:]), sdk.PublicKeyFromBytes(signer.FeePayer[32:])
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.route_fee_payer_shards(cluster,fee_payer,enabled,minimum_balance_lamports,maximum_balance_lamports,rolling_window_seconds,maximum_window_spend_lamports,maximum_transaction_fee_lamports) VALUES($1,$2,true,1000000,100000000,3600,50000000,100000)`, a.Lease.Cluster, payerKey.String()); err != nil {
		t.Fatal(err)
	}
	// The same ALT-backed message, paid by the fee-only payer: the payer is
	// the writable first signer and the delegate a read-only signer.
	table := sdk.MustPublicKeyFromBase58(a.SelectedALTs[0].Address)
	message := []byte{0x80, 2, 1, 1, 3}
	message = append(message, payerKey[:]...)
	message = append(message, delegateKey[:]...)
	message = append(message, sdk.SystemProgramID[:]...)
	blockhash := sha256.Sum256([]byte(a.Lease.IdempotencyKey))
	message = append(message, blockhash[:]...)
	message = append(message, 1, 2, 1, 1, 1, 0xAA, 1)
	message = append(message, table[:]...)
	message = append(message, 1, 0, 0)
	unsigned := append(append([]byte{2}, make([]byte, 128)...), message...)
	mh, wh := sha256.Sum256(message), sha256.Sum256(unsigned)
	tx := &a.Preparation.Transaction
	tx.Message, tx.UnsignedWire, tx.PacketBytes = message, unsigned, len(unsigned)
	tx.MessageSHA256, tx.WireSHA256 = hex.EncodeToString(mh[:]), hex.EncodeToString(wh[:])
	tx.WritableAccounts = []string{payerKey.String(), a.SelectedALTs[0].Addresses[0]}
	tx.FeeLamports = 10000
	a.Preparation.Simulation.WireSHA256 = tx.WireSHA256
	a.FeePayer = payerKey.String()
	signer.FeeOnly = []ed25519.PrivateKey{payer}
	worker, err := NewWorker(Config{Cluster: a.Lease.Cluster, Owner: a.Lease.Owner, LeaseTTL: time.Minute, BatchSize: 1, TickInterval: time.Second, Facts: testFacts()}, store, &countingChain{}, &fakeStatus{}, signer)
	if err != nil {
		t.Fatal(err)
	}
	// Rust's admission: a balance that would end below the shard floor is a
	// reselection, and nothing is persisted.
	worker.balances = shardBalance(1_005_000)
	if _, err := worker.ExecuteFresh(ctx, a); !errors.Is(err, errFeePayerReselection) {
		t.Fatalf("below-floor shard admitted: %v", err)
	}
	worker.balances = shardBalance(50_000_000)
	id, err := worker.ExecuteFresh(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var kind, feePayer string
	var reserved, balance int64
	if err := pool.QueryRow(ctx, `SELECT s.fee_payer_kind,s.fee_payer,r.compiled_fee_lamports,r.observed_balance_lamports FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.route_fee_payer_spend_reservations r ON r.signed_submission_id=s.id WHERE s.id=$1`, id).Scan(&kind, &feePayer, &reserved, &balance); err != nil {
		t.Fatal(err)
	}
	if kind != "fee_only_shard" || feePayer != payerKey.String() || reserved != 10000 || balance != 50_000_000 {
		t.Fatalf("fee-only publication %s %s %d %d", kind, feePayer, reserved, balance)
	}
}

// shardBalance answers every confirmed balance read with one lamport amount.
type shardBalance uint64

func (b shardBalance) Accounts(_ context.Context, keys []sdk.PublicKey, _ rpc.CommitmentType, slot uint64) (uint64, []*chain.Account, error) {
	accounts := make([]*chain.Account, len(keys))
	for i, key := range keys {
		accounts[i] = &chain.Account{Key: key, Owner: sdk.SystemProgramID, Lamports: uint64(b)}
	}
	return slot + 1, accounts, nil
}
