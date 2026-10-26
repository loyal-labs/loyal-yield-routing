package autodeposit

import (
	"context"
	"errors"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// scriptedControllerChain replays observations per signature for the
// controller's recovery path.
type scriptedControllerChain struct {
	balances     map[string]int64
	observations map[string]AttemptObservation
	receipts     map[string]ReceiptEvidence
	positions    map[string][2]int64
	// lamports scripts account balances; an unscripted payer holds one SOL.
	lamports     map[string]uint64
	allowanceErr error
	simulateErr  error
	broadcastErr error
	// The scripted cluster: sent signatures, the last one asked about, and the
	// finalized height (zero never expires a blockhash).
	sent          map[string]bool
	lastSignature string
	height        uint64
}

func (s *scriptedControllerChain) ConfirmedTokenBalanceRaw(ctx context.Context, tokenAccount, authority string) (int64, error) {
	amount, known := s.balances[tokenAccount]
	if !known {
		return 0, ErrTokenAccountAbsent
	}
	return amount, nil
}

func (s *scriptedControllerChain) ConfirmedLamports(_ context.Context, address string) (uint64, error) {
	if lamports, scripted := s.lamports[address]; scripted {
		return lamports, nil
	}
	return 1_000_000_000, nil
}

func (s *scriptedControllerChain) RemainingDelegationAllowanceRaw(ctx context.Context, delegation string, identity DelegationIdentity) (int64, error) {
	if s.allowanceErr != nil {
		return 0, s.allowanceErr
	}
	return 1 << 40, nil
}

func (s *scriptedControllerChain) LatestBlockhash(ctx context.Context) (Blockhash, error) {
	return Blockhash{Hash: "itest-controller-blockhash", LastValidBlockHeight: 900}, nil
}

func (s *scriptedControllerChain) FinalizedBlockHeight(context.Context) (uint64, uint64, error) {
	return s.height, 1, nil
}

// SignatureState answers with the scripted observation; a controller-built
// signature is unseen until its bytes are sent.
func (s *scriptedControllerChain) SignatureState(_ context.Context, signature string) (chain.SignatureState, error) {
	s.lastSignature = signature
	unseen := chain.SignatureState{ContextSlot: 1}
	if strings.HasPrefix(signature, "itest-controller-") && !s.sent[signature] {
		return unseen, nil
	}
	observation, seen := s.observations[signature]
	if !seen {
		return unseen, nil
	}
	switch observation.State {
	case AttemptConfirmed:
		return chain.SignatureState{Found: true, Slot: uint64(*observation.ConfirmedSlot), Commitment: chain.Confirmed, ContextSlot: 1}, nil
	case AttemptFailed:
		return chain.SignatureState{Found: true, Slot: 1, Commitment: chain.Confirmed, Err: "scripted failure", ContextSlot: 1}, nil
	}
	return unseen, nil
}

// SendWire lands the bytes of the signature land asked about last.
func (s *scriptedControllerChain) SendWire(context.Context, []byte, bool) error {
	if s.sent == nil {
		s.sent = map[string]bool{}
	}
	s.sent[s.lastSignature] = true
	if s.broadcastErr != nil {
		return s.broadcastErr
	}
	for _, effect := range s.receipts[s.lastSignature].Effects {
		s.balances[effect.TokenAccount] = effect.PostRaw
	}
	return nil
}

func (s *scriptedControllerChain) ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error) {
	receipt, seen := s.receipts[signature]
	if !seen {
		return ReceiptEvidence{}, errors.New("no scripted receipt")
	}
	return receipt, nil
}

func (s *scriptedControllerChain) SimulateExact(ctx context.Context, attempt DurableAttempt) error {
	return s.simulateErr
}

func (s *scriptedControllerChain) ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute, _ int64) (VaultPosition, error) {
	if position, seen := s.positions[plan.Reserve]; seen {
		return VaultPosition{AmountRaw: position[0], ObservedSlot: position[1]}, nil
	}
	return VaultPosition{AmountRaw: plan.AmountRaw, ObservedSlot: 870_002}, nil
}
