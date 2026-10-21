package chain

import (
	"context"
	"errors"
	"time"
)

type Commitment int

const (
	Processed Commitment = iota + 1
	Confirmed
	Finalized
)

type SignatureState struct {
	Found       bool
	Slot        uint64
	Commitment  Commitment
	Err         string
	ContextSlot uint64
}

type LandChain interface {
	SendWire(ctx context.Context, wire []byte, skipPreflight bool) error
	FinalizedBlockHeight(ctx context.Context) (height, slot uint64, err error)
	SignatureState(ctx context.Context, signature string) (SignatureState, error)
}

type Attempt struct {
	Wire                 []byte
	Signature            string
	LastValidBlockHeight uint64
	Required             Commitment
}

type OutcomeKind int

const (
	Landed OutcomeKind = iota + 1
	Failed
	Expired
)

var LandResendEvery = time.Second

type Outcome struct {
	Kind        OutcomeKind
	Slot        uint64
	Commitment  Commitment
	Err         string
	BlockHeight uint64
	ContextSlot uint64
	SendErr     error
}

// Observe classifies the signature once, without sending. Height is read
// before status: if the signature is still absent after a finalized height
// beyond its expiry was read, it can never land. The absence counts only from
// a node that has reached that finalized slot; a lagging node behind a load
// balancer may not know the signature landed.
func Observe(ctx context.Context, chain LandChain, attempt Attempt) (Outcome, error) {
	out, _, err := observe(ctx, chain, attempt)
	return out, err
}

func observe(ctx context.Context, chain LandChain, attempt Attempt) (Outcome, bool, error) {
	height, finalizedSlot, err := chain.FinalizedBlockHeight(ctx)
	if err != nil {
		return Outcome{}, false, err
	}
	status, err := chain.SignatureState(ctx, attempt.Signature)
	if err != nil {
		return Outcome{}, false, err
	}
	switch {
	case status.Found && status.Commitment >= Confirmed && status.Err != "":
		return Outcome{Kind: Failed, Slot: status.Slot, Commitment: status.Commitment, Err: status.Err}, false, nil
	case status.Found && status.Commitment >= attempt.Required && status.Err == "":
		return Outcome{Kind: Landed, Slot: status.Slot, Commitment: status.Commitment}, false, nil
	case !status.Found && height > attempt.LastValidBlockHeight && status.ContextSlot >= finalizedSlot:
		return Outcome{Kind: Expired, BlockHeight: height, ContextSlot: status.ContextSlot}, false, nil
	case !status.Found && height > attempt.LastValidBlockHeight:
		return Outcome{}, false, nil
	}
	return Outcome{}, !status.Found || status.Commitment == Processed && status.Err == "", nil
}

// Land resends the same bytes until the signature lands, fails on chain, or
// its blockhash expires. Solana drops forwarded transactions, so one send is
// not enough; resending identical bytes cannot spend twice because the
// signature is the transaction's identity.
//
// recordSend runs before every send and must durably count it on the
// operation row; if it fails, nothing is sent. Every send keeps preflight, so
// the cluster never forwards bytes that fail at its current state: such a wire
// expires without effect instead of failing on chain. A send error, including
// a preflight refusal or an already-processed answer, is not an outcome: only
// the signature status and the block height decide.
func Land(ctx context.Context, chain LandChain, attempt Attempt, every time.Duration, recordSend func(context.Context) error) (Outcome, error) {
	if len(attempt.Wire) == 0 || attempt.Signature == "" || attempt.LastValidBlockHeight == 0 || every <= 0 || recordSend == nil {
		return Outcome{}, errors.New("land requires signed bytes, signature, expiry, interval and send record")
	}
	if attempt.Required != Confirmed && attempt.Required != Finalized {
		return Outcome{}, errors.New("land requires confirmed or finalized commitment")
	}
	var sendErr error
	for {
		out, send, err := observe(ctx, chain, attempt)
		if err != nil {
			return Outcome{}, err
		}
		if out.Kind != 0 {
			out.SendErr = sendErr
			return out, nil
		}
		if send {
			if err := recordSend(ctx); err != nil {
				return Outcome{}, err
			}
			if err := chain.SendWire(ctx, attempt.Wire, false); err != nil {
				sendErr = err
			}
		}
		timer := time.NewTimer(every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Outcome{}, ctx.Err()
		case <-timer.C:
		}
	}
}
