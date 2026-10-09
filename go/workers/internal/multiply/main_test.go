package multiply

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

func TestMain(m *testing.M) {
	landResendEvery = time.Millisecond
	os.Exit(m.Run())
}

func testFacts() *engine.Facts { return engine.NewFacts(prometheus.NewRegistry()) }

// fakeChain is a cluster that answers every read at slot, simulates
// successfully, has no receipts (or fails reading them with receiptErr) and
// records sends; landing sees no signature
// and the finalized height.
type fakeChain struct {
	genesis, blockhash solana.Hash
	lastValid, slot    uint64
	fee, height        uint64
	accounts           map[solana.PublicKey]*chain.Account
	sent               [][]byte
	receiptErr         error
}

func (f *fakeChain) GenesisHash(context.Context) (solana.Hash, error) { return f.genesis, nil }
func (f *fakeChain) Blockhash(context.Context, rpc.CommitmentType, uint64) (solana.Hash, uint64, uint64, error) {
	return f.blockhash, f.lastValid, f.slot, nil
}
func (f *fakeChain) Accounts(_ context.Context, keys []solana.PublicKey, _ rpc.CommitmentType, _ uint64) (uint64, []*chain.Account, error) {
	accounts := make([]*chain.Account, len(keys))
	for i, key := range keys {
		accounts[i] = f.accounts[key]
	}
	return f.slot, accounts, nil
}
func (f *fakeChain) Fee(context.Context, []byte, rpc.CommitmentType, uint64) (uint64, error) {
	return f.fee, nil
}
func (f *fakeChain) Simulate(context.Context, []byte, rpc.SimulateTransactionOpts) (chain.Simulated, error) {
	return chain.Simulated{Slot: f.slot}, nil
}
func (f *fakeChain) Receipt(context.Context, solana.Signature, rpc.CommitmentType) (chain.Receipt, error) {
	if f.receiptErr != nil {
		return chain.Receipt{}, f.receiptErr
	}
	return chain.Receipt{}, chain.ErrNotFound
}
func (f *fakeChain) SendWire(_ context.Context, wire []byte, _ bool) error {
	f.sent = append(f.sent, wire)
	return nil
}
func (f *fakeChain) FinalizedBlockHeight(context.Context) (uint64, uint64, error) {
	return f.height, f.slot, nil
}
func (f *fakeChain) SignatureState(context.Context, string) (chain.SignatureState, error) {
	return chain.SignatureState{ContextSlot: f.slot}, nil
}
