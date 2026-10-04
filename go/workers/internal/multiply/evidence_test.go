package multiply

import (
	"context"
	"testing"
)

type expiryFixtureRPC struct {
	fakeRPC
	finalizedHeight, finalizedSlot, historySlot, firstAvailable uint64
	history                                                     *SignatureObservation
}

func (r *expiryFixtureRPC) FinalizedBlockHeight(context.Context) (uint64, error) {
	return r.finalizedHeight, nil
}
func (r *expiryFixtureRPC) FinalizedSlot(context.Context) (uint64, error) {
	return r.finalizedSlot, nil
}
func (r *expiryFixtureRPC) HistoricalSignature(context.Context, string) (*SignatureObservation, uint64, error) {
	return r.history, r.historySlot, nil
}
func (r *expiryFixtureRPC) FirstAvailableBlock(context.Context) (uint64, error) {
	return r.firstAvailable, nil
}

func TestNoEffectProofRequiresOriginalEvidenceAndCompleteHistory(t *testing.T) {
	topology, err := DeriveEarnMaxTopology(fixtureKey(61), 320)
	if err != nil {
		t.Fatal(err)
	}
	balance := TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 1000}
	before := &ObservedRoute{Slot: 100, Claim: balance}
	op := integrationOperation("unit-expiry", 1, 1)
	op.ExpectedEffects = ExpectedEffects{TokenAmountsBefore: []TokenAmountBefore{{Account: balance.Account, Mint: balance.Mint, AmountRaw: balance.AmountRaw}}, TokenDeltas: []TokenDelta{{Account: balance.Account, Mint: balance.Mint, RawDelta: -100}}}
	pre, err := NewOperationPrestate(op, before, topology)
	if err != nil {
		t.Fatal(err)
	}
	signed := signedWireFixture(t, false)
	mh, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	op.SignedWire = signed.Wire
	op.SignedWireSHA256 = &signed.WireSHA256
	op.TransactionSignature = &signed.TransactionSignature
	op.RecentBlockhash = &signed.RecentBlockhash
	op.MessageSHA256 = &mh
	expiry := uint64(signed.LastValidBlockHeight)
	op.LastValidBlockHeight = &expiry
	pre.Signature = signed.TransactionSignature
	pre.WireSHA256 = signed.WireSHA256
	after := &ObservedRoute{Slot: 200, Claim: balance}
	rpc := &expiryFixtureRPC{fakeRPC: fakeRPC{genesis: mainnetGenesisHash}, finalizedHeight: expiry + 1, finalizedSlot: 199, historySlot: 200, firstAvailable: 100}
	executor, err := NewExecutor(rpc, testDelegateSeed())
	if err != nil {
		t.Fatal(err)
	}
	// These exercise the proof validator's refusal conditions only. The signed
	// parser fixture and controlled RPC are not claimed as chain execution proof.
	cases := []struct {
		name   string
		mutate func(*expiryFixtureRPC, *ObservedRoute, *OperationPrestateEvidence)
	}{
		{"unexpired", func(r *expiryFixtureRPC, _ *ObservedRoute, _ *OperationPrestateEvidence) { r.finalizedHeight = expiry }},
		{"history-pruned", func(r *expiryFixtureRPC, _ *ObservedRoute, _ *OperationPrestateEvidence) { r.firstAvailable = 101 }},
		{"history-stale", func(r *expiryFixtureRPC, _ *ObservedRoute, _ *OperationPrestateEvidence) { r.historySlot = 199 }},
		{"signature-found", func(r *expiryFixtureRPC, _ *ObservedRoute, _ *OperationPrestateEvidence) {
			r.history = &SignatureObservation{Slot: 150, ConfirmationState: "confirmed"}
		}},
		{"effect-changed", func(_ *expiryFixtureRPC, a *ObservedRoute, _ *OperationPrestateEvidence) { a.Claim.AmountRaw-- }},
		{"snapshot-before-finality", func(r *expiryFixtureRPC, _ *ObservedRoute, _ *OperationPrestateEvidence) { r.finalizedSlot = 201 }},
		{"original-slot-missing", func(_ *expiryFixtureRPC, _ *ObservedRoute, p *OperationPrestateEvidence) { p.ObservedSlot = 0 }},
		{"original-hash-drift", func(_ *expiryFixtureRPC, _ *ObservedRoute, p *OperationPrestateEvidence) {
			p.FinancialAnchorsSHA256 = PolicyDataHash([]byte("drift"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := *rpc
			a := *after
			p := *pre
			tc.mutate(&r, &a, &p)
			executor.RPC = &r
			if _, err := executor.ProveExpiredNoEffect(context.Background(), op, &a, topology, 199, &p); err == nil {
				t.Fatal("uncertain expiry released ownership")
			}
		})
	}
	executor.RPC = rpc
	if _, err := executor.ProveExpiredNoEffect(context.Background(), op, after, topology, 199, nil); err == nil {
		t.Fatal("legacy attempt acquired reconstructed proof")
	}
}
