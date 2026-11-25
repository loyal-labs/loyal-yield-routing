package backyard

import (
	"context"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type feeCapitalFixture struct {
	balance, obligation solana.PublicKey
	x                   chain.Execution
	first               uint64
	calls               int
}

func (c *feeCapitalFixture) FirstAvailableBlock(context.Context) (uint64, error) { return c.first, nil }
func (c *feeCapitalFixture) History(_ context.Context, _ solana.PublicKey, _ int, _ solana.Signature, _ rpc.CommitmentType, _ uint64) ([]chain.Signed, error) {
	return []chain.Signed{{Slot: c.x.Slot, Signature: c.x.Transaction.Signatures[0]}}, nil
}
func (c *feeCapitalFixture) Execution(context.Context, solana.Signature, rpc.CommitmentType) (chain.Execution, error) {
	c.calls++
	return c.x, nil
}

func TestLoopingFeeUnjournaledCollateralAndDebtPrincipal(t *testing.T) {
	lane := looping.routes[0]
	c := feeCapitalFixture{balance: solanaKey(lane.CollateralCustody), obligation: solanaKey(lane.Kamino.Obligation)}
	var sig solana.Signature
	sig[0] = 23
	keys := []solana.PublicKey{kamino.ProgramID, c.balance, c.obligation, solana.SystemProgramID}
	c.x = chain.Execution{Transaction: &solana.Transaction{Signatures: []solana.Signature{sig}}, Receipt: chain.Receipt{Slot: 505, Keys: keys,
		Pre: map[solana.PublicKey]chain.TokenBalance{c.balance: {Amount: 11}}, Post: map[solana.PublicKey]chain.TokenBalance{c.balance: {Amount: 11}}}}
	watched := map[solana.PublicKey]bool{c.balance: false, c.obligation: true}
	check := func(known map[string]bool, want string) {
		t.Helper()
		c.calls = 0
		got, err := checkFeeCapital(t.Context(), &c, watched, 500, 510, known)
		if err != nil || got != want || c.calls > 2 {
			t.Fatalf("reason=%s reads=%d err=%v", got, c.calls, err)
		}
	}
	c.x.Transaction.Message.Instructions = []solana.CompiledInstruction{{ProgramIDIndex: 0, Accounts: []uint16{2}, Data: kamino.RefreshObligationDiscriminator[:]}}
	check(nil, "")
	c.x.Post[c.balance] = chain.TokenBalance{Amount: 12}
	check(nil, sig.String())
	check(map[string]bool{sig.String(): true}, "")
	c.x.Post[c.balance] = chain.TokenBalance{Amount: 11}
	c.x.Transaction.Message.Instructions = nil
	c.x.Inner = []rpc.InnerInstruction{{Index: 0, Instructions: []rpc.CompiledInstruction{{ProgramIDIndex: 0, Accounts: []uint16{2}, Data: kamino.RepayV2Discriminator[:]}}}}
	check(nil, sig.String())
	// An untraced third-party wrapper touching the obligation cannot prove a refresh.
	c.x.Inner = nil
	c.x.Transaction.Message.Instructions = []solana.CompiledInstruction{{ProgramIDIndex: 3, Accounts: []uint16{2}}}
	check(nil, sig.String())
	c.first = 501
	if _, err := checkFeeCapital(t.Context(), &c, watched, 500, 510, nil); err == nil {
		t.Fatal("truncated archive accepted")
	}
	f := feeAnchor(t, feeMonday, 10)
	s := feeBank(f, feeMonday+3600, 1024, 1024, 0)
	s.feeCapitalHold = sig.String()
	held := feeStep(t, f, s, 1224, 1224, 0, 0, true)
	if held.Assessed != 0 || held.After.PublishedValue != 1224 || held.After.CapitalHold != sig.String() || held.After.CapitalSlot != f.CapitalSlot {
		t.Fatal("outside principal charged or NAV frozen", held)
	}
	s.feeCapitalHold = ""
	s.feeCapitalSlot = 0
	if _, err := f.assess(s, 1224, 1224, 0, 0, true); err == nil {
		t.Fatal("unsigned history claim admitted a fee")
	}
}

func TestLoopingFeeReceiptResolutionPreservesLossCarryforward(t *testing.T) {
	at := feeMonday
	f := feeAnchor(t, at, 100)
	f = feeStep(t, f, feeBank(f, at+3600, 1024, f.PublishedValue, 0), 824, 824, 0, 0, true).After
	var signature solana.Signature
	signature[0] = 45
	s := feeBank(f, at+7200, 1024, f.PublishedValue, 0)
	s.feeCapitalHold = signature.String()
	held := feeStep(t, f, s, 924, 924, 0, 0, true)
	f = held.After
	s = feeBank(f, at+10800, 1024, f.PublishedValue, 0)
	s.feeResolution = feeCapitalResolution{Signature: signature.String(), Slot: held.Slot, Principal: 100, Supply: 1024}
	e := feeStep(t, f, s, 974, 974, 0, 0, true)
	if e.Assessed != 0 || e.Resolution != s.feeResolution || e.After.CapitalHold != "" || e.After.PeakBits != feePriceBits(1124, 1024).String() {
		t.Fatal("waiver erased old drawdown or charged outside capital", e)
	}
	f = e.After
	e = feeStep(t, f, feeBank(f, at+14400, 1024, f.PublishedValue, 0), 1124, 1124, 0, 0, true)
	if e.Assessed != 0 {
		t.Fatal("charged recovery", e)
	}
	f = e.After
	e = feeStep(t, f, feeBank(f, at+18000, 1024, f.PublishedValue, 0), 1224, 1224, 0, 0, true)
	if e.Assessed != 10 {
		t.Fatal("routine fee failed to resume", e)
	}
}
