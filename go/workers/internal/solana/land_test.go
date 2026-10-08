package solana

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// fakeCluster drops the first `drops` forwards, like a leader that never
// receives them, then confirms the signature on the next send.
type fakeCluster struct {
	drops     int
	height    uint64
	heightInc uint64
	failWith  string
	sent      [][]byte
	preflight []bool
	landedAt  uint64
	processed string
	// finalizedSlot is the slot of the finalized height; a status read
	// below it comes from a lagging node.
	finalizedSlot uint64
}

func (f *fakeCluster) SendWire(_ context.Context, wire []byte, skipPreflight bool) error {
	f.sent = append(f.sent, append([]byte(nil), wire...))
	f.preflight = append(f.preflight, !skipPreflight)
	if len(f.sent) <= f.drops {
		return errors.New("forward dropped")
	}
	f.landedAt = 77
	return nil
}

func (f *fakeCluster) FinalizedBlockHeight(context.Context) (uint64, uint64, error) {
	f.height += f.heightInc
	return f.height, f.finalizedSlot, nil
}

func (f *fakeCluster) SignatureState(context.Context, string) (SignatureState, error) {
	if f.processed != "" {
		return SignatureState{Found: true, Slot: 5, Commitment: Processed, Err: f.processed, ContextSlot: 9}, nil
	}
	if f.landedAt == 0 {
		return SignatureState{ContextSlot: 9}, nil
	}
	return SignatureState{Found: true, Slot: f.landedAt, Commitment: Confirmed, Err: f.failWith, ContextSlot: 80}, nil
}

func TestLandResendsSameBytesUntilDroppedForwardLands(t *testing.T) {
	// The Rust confirmer rebroadcast exists because Solana drops forwards: a
	// single send left routes stuck until expiry.
	chain := &fakeCluster{drops: 2, height: 100, heightInc: 1}
	wire := []byte{1, 2, 3}
	recorded := 0
	out, err := Land(context.Background(), chain, Attempt{Wire: wire, Signature: "sig", LastValidBlockHeight: 150, Required: Confirmed}, time.Millisecond, func(context.Context) error { recorded++; return nil })
	if err != nil || out.Kind != Landed || out.Slot != 77 {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if len(chain.sent) != 3 || recorded != 3 {
		t.Fatalf("sent %d recorded %d", len(chain.sent), recorded)
	}
	for _, sent := range chain.sent {
		if !bytes.Equal(sent, wire) {
			t.Fatal("resend changed the bytes")
		}
	}
	if !chain.preflight[0] || chain.preflight[1] || chain.preflight[2] {
		t.Fatalf("preflight %v; only the first send keeps preflight", chain.preflight)
	}
}

func TestLandExpiresWhenHeightPassesWithoutSignature(t *testing.T) {
	chain := &fakeCluster{drops: 1 << 30, height: 148, heightInc: 1}
	out, err := Land(context.Background(), chain, Attempt{Wire: []byte{1}, Signature: "sig", LastValidBlockHeight: 150, Required: Confirmed}, time.Millisecond, func(context.Context) error { return nil })
	if err != nil || out.Kind != Expired || out.BlockHeight != 151 || out.ContextSlot != 9 || out.SendErr == nil {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if len(chain.sent) != 2 {
		t.Fatalf("sent %d times; nothing is sent once the height passed expiry", len(chain.sent))
	}
}

func TestLandReportsChainFailure(t *testing.T) {
	chain := &fakeCluster{height: 100, failWith: `{"InstructionError":[0,"Custom"]}`}
	out, err := Land(context.Background(), chain, Attempt{Wire: []byte{1}, Signature: "sig", LastValidBlockHeight: 150, Required: Finalized}, time.Millisecond, func(context.Context) error { return nil })
	if err != nil || out.Kind != Failed || out.Slot != 77 || out.Err == "" {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

func TestLandSendsNothingWhenSendCannotBeRecorded(t *testing.T) {
	chain := &fakeCluster{height: 100}
	_, err := Land(context.Background(), chain, Attempt{Wire: []byte{1}, Signature: "sig", LastValidBlockHeight: 150, Required: Confirmed}, time.Millisecond, func(context.Context) error { return errors.New("stale row") })
	if err == nil || len(chain.sent) != 0 {
		t.Fatalf("err %v sent %d", err, len(chain.sent))
	}
}

func TestLandWaitsOnProcessedErrorWithoutResending(t *testing.T) {
	chain := &fakeCluster{height: 100, processed: "fork error"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Land(ctx, chain, Attempt{Wire: []byte{1}, Signature: "sig", LastValidBlockHeight: 150, Required: Confirmed}, time.Millisecond, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || len(chain.sent) != 0 {
		t.Fatalf("err %v sent %d", err, len(chain.sent))
	}
}

func TestLandDoesNotExpireOnALaggingNodesAbsence(t *testing.T) {
	// Behind a load balancer the status node can trail the node that served
	// the finalized height; its "absent" says nothing about a landed signature.
	chain := &fakeCluster{drops: 1 << 30, height: 200, finalizedSlot: 10}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Land(ctx, chain, Attempt{Wire: []byte{1}, Signature: "sig", LastValidBlockHeight: 150, Required: Confirmed}, time.Millisecond, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || len(chain.sent) != 0 {
		t.Fatalf("err %v sent %d; a lagging absence must neither expire nor resend expired bytes", err, len(chain.sent))
	}
}
