package chain

import (
	"context"
	"errors"
	"time"
)

// Commitment is the cluster's confirmation level for one signature.
type Commitment int

const (
	Processed Commitment = iota + 1
	Confirmed
	Finalized
)

// SignatureState is one exact signature's status, read with full history.
// Found is false when the cluster has no record of the signature.
type SignatureState struct {
	Found      bool
	Slot       uint64
	Commitment Commitment
	// Err is the transaction's chain error, empty when it succeeded.
	Err string
	// ContextSlot is the slot the status answer was read at.
	ContextSlot uint64
}

// LandChain is the RPC surface land needs. SendWire submits the exact bytes
// once with maxRetries 0; it never retries on its own.
type LandChain interface {
	SendWire(ctx context.Context, wire []byte, skipPreflight bool) error
	// FinalizedBlockHeight returns the finalized block height and the slot
	// it belongs to, read together from one node.
	FinalizedBlockHeight(ctx context.Context) (height, slot uint64, err error)
	SignatureState(ctx context.Context, signature string) (SignatureState, error)
}

// Attempt is a signed transaction already written to its operation row with
// its blockhash expiry.
type Attempt struct {
	Wire                 []byte
	Signature            string
	LastValidBlockHeight uint64
	// Required is the commitment the family treats as landed.
	Required Commitment
}

type OutcomeKind int

const (
	// Landed: the signature reached the required commitment without error.
	Landed OutcomeKind = iota + 1
	// Failed: the signature is confirmed with a chain error.
	Failed
	// Expired: the finalized block height passed the blockhash's last valid
	// height and the cluster has no record of the signature, so these bytes
	// can never land.
	Expired
)

type Outcome struct {
	// Kind is zero while the signature is still undecided.
	Kind OutcomeKind
	// Slot is the landed or failed slot, Commitment the level it was read at.
	Slot       uint64
	Commitment Commitment
	Err        string
	// BlockHeight is the finalized height that proved expiry.
	BlockHeight uint64
	// ContextSlot is the slot at which the absent status was read.
	ContextSlot uint64
	// SendErr is the last send transport error, kept for the log only.
	SendErr error
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

// observe also reports whether resending the bytes can still help.
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
		// Expired bytes are not sent again; wait for a node that is caught up.
		return Outcome{}, false, nil
	}
	// Not seen yet, or seen only on a fork that may be dropped. A processed
	// error or a confirmed success awaiting finalization waits without sending.
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
