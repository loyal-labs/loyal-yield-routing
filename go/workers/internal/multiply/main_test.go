package multiply

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMain(m *testing.M) {
	landResendEvery = time.Millisecond
	os.Exit(m.Run())
}

func testFacts() *engine.Facts { return engine.NewFacts(prometheus.NewRegistry()) }

// surfaceChain lands through a test's RPC surface fake or local bank.
type surfaceChain struct{ rpc RPCSurface }

func (c surfaceChain) SendWire(ctx context.Context, wire []byte, _ bool) error {
	_, err := c.rpc.SendRawTransaction(ctx, wire)
	return err
}

func (c surfaceChain) FinalizedBlockHeight(ctx context.Context) (uint64, uint64, error) {
	height, err := c.rpc.BlockHeight(ctx)
	return height, 1, err
}

func (c surfaceChain) SignatureState(ctx context.Context, signature string) (chain.SignatureState, error) {
	observation, err := c.rpc.SignatureStatus(ctx, signature)
	if err != nil || observation == nil {
		return chain.SignatureState{ContextSlot: 1}, err
	}
	state := chain.SignatureState{Found: true, Slot: uint64(observation.Slot), ContextSlot: 1, Commitment: chain.Processed}
	switch observation.ConfirmationState {
	case "confirmed":
		state.Commitment = chain.Confirmed
	case "finalized":
		state.Commitment = chain.Finalized
	}
	if observation.Err != nil {
		state.Err = *observation.Err
	}
	return state, nil
}
