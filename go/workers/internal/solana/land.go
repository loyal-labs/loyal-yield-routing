package solana

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
	FinalizedBlockHeight(ctx context.Context) (uint64, error)
	SignatureState(ctx context.Context, signature string) (SignatureState, error)
}

// Attempt is a signed transaction already written to its operation row with
// its blockhash expiry. Sends counts the sends that row already records.
type Attempt struct {
	Wire                 []byte
	Signature            string
	LastValidBlockHeight uint64
	Sends                int
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
	Kind OutcomeKind
	// Slot is the landed or failed slot.
	Slot uint64
	Err  string
	// BlockHeight is the finalized height that proved expiry.
	BlockHeight uint64
	// ContextSlot is the slot at which the absent status was read.
	ContextSlot uint64
	// SendErr is the last send transport error, kept for the log only.
	SendErr error
}

// Land resends the same bytes until the signature lands, fails on chain, or
// its blockhash expires. Solana drops forwarded transactions, so one send is
// not enough; resending identical bytes cannot spend twice because the
// signature is the transaction's identity.
//
// recordSend runs before every send and must durably count it on the
// operation row; if it fails, nothing is sent. A send error is not an outcome:
// only the signature status and the block height decide.
func Land(ctx context.Context, chain LandChain, attempt Attempt, every time.Duration, recordSend func(context.Context) error) (Outcome, error) {
	if len(attempt.Wire) == 0 || attempt.Signature == "" || attempt.LastValidBlockHeight == 0 || every <= 0 || recordSend == nil {
		return Outcome{}, errors.New("land requires signed bytes, signature, expiry, interval and send record")
	}
	if attempt.Required != Confirmed && attempt.Required != Finalized {
		return Outcome{}, errors.New("land requires confirmed or finalized commitment")
	}
	var sendErr error
	for {
		// Height before status: if the signature is still absent after a
		// finalized height beyond its expiry was read, it can never land.
		height, err := chain.FinalizedBlockHeight(ctx)
		if err != nil {
			return Outcome{}, err
		}
		status, err := chain.SignatureState(ctx, attempt.Signature)
		if err != nil {
			return Outcome{}, err
		}
		switch {
		case status.Found && status.Commitment >= Confirmed && status.Err != "":
			return Outcome{Kind: Failed, Slot: status.Slot, Err: status.Err}, nil
		case status.Found && status.Commitment >= attempt.Required && status.Err == "":
			return Outcome{Kind: Landed, Slot: status.Slot}, nil
		case !status.Found && height > attempt.LastValidBlockHeight:
			return Outcome{Kind: Expired, BlockHeight: height, ContextSlot: status.ContextSlot, SendErr: sendErr}, nil
		case !status.Found || status.Commitment == Processed && status.Err == "":
			// Not seen yet, or seen only on a fork that may be dropped.
			if err := recordSend(ctx); err != nil {
				return Outcome{}, err
			}
			// The first send keeps preflight; resends of the same bytes skip it.
			if err := chain.SendWire(ctx, attempt.Wire, attempt.Sends > 0); err != nil {
				sendErr = err
			}
			attempt.Sends++
		}
		// A processed error or a confirmed success awaiting finalization
		// waits without sending.
		timer := time.NewTimer(every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Outcome{}, ctx.Err()
		case <-timer.C:
		}
	}
}
