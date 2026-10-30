package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type foreignMemoryStore struct {
	checkpoints map[string]foreignCheckpoint
	writes      []foreignEvidence
}

func (m *foreignMemoryStore) foreignCheckpoint(_ context.Context, _, a string) (foreignCheckpoint, bool, error) {
	c, ok := m.checkpoints[a]
	return c, ok, nil
}
func (m *foreignMemoryStore) saveForeignEvidence(_ context.Context, _, a string, e foreignEvidence) error {
	c, ok := m.checkpoints[a]
	if ok && c != e.Before && c != e.After {
		return fmt.Errorf("checkpoint conflict")
	}
	m.checkpoints[a] = e.After
	m.writes = append(m.writes, e)
	return nil
}

type foreignChainFixture struct {
	history    map[solana.PublicKey][]chain.Signed
	executions map[solana.Signature]chain.Execution
	finalized  uint64
}

func (f *foreignChainFixture) Slot(context.Context, rpc.CommitmentType) (uint64, error) {
	return f.finalized, nil
}
func (f *foreignChainFixture) Execution(_ context.Context, s solana.Signature, c rpc.CommitmentType) (chain.Execution, error) {
	x, ok := f.executions[s]
	if !ok || (c == rpc.CommitmentFinalized && x.Slot > f.finalized) {
		return chain.Execution{}, chain.ErrNotFound
	}
	return x, nil
}
func (f *foreignChainFixture) History(_ context.Context, a solana.PublicKey, n int, b solana.Signature, _ rpc.CommitmentType, _ uint64) ([]chain.Signed, error) {
	rows := f.history[a]
	if !b.IsZero() {
		found := false
		for i, r := range rows {
			if r.Signature == b {
				rows = rows[i+1:]
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cursor not found")
		}
	}
	return rows[:min(n, len(rows))], nil
}

func TestForeignInflowFinalityRestartAndRollback(t *testing.T) {
	address := rwa.idleATA
	ctx := context.Background()
	anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
	db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{address: anchor}}
	x := foreignFixture(address, 17, 1)
	signature := x.Transaction.Signatures[0]
	c := &foreignChainFixture{finalized: x.Slot, executions: map[solana.Signature]chain.Execution{signature: x}, history: map[solana.PublicKey][]chain.Signed{solanaKey(address): {{Signature: signature, Slot: x.Slot}, {Signature: solana.MustSignatureFromBase58(foreignInflowAnchor), Slot: anchor.Slot}}}}
	read := func() foreignCheckpoint {
		t.Helper()
		got, err := replayForeignInflows(ctx, db, c, rwa, address, c.finalized+10, c.finalized, "")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got.ForeignRaw != 1 || got != db.checkpoints[address] {
		t.Fatalf("finalized gift not journaled: %+v", got)
	}
	if got := read(); got.ForeignRaw != 1 || len(db.writes) != 1 {
		t.Fatal("restart double-counted gift")
	}
	y := foreignFixture(address, 18, 2)
	y.Slot++
	y.Transaction.Signatures[0] = solana.Signature{2}
	c.executions[y.Transaction.Signatures[0]] = y
	c.history[solanaKey(address)] = append([]chain.Signed{{Signature: y.Transaction.Signatures[0], Slot: y.Slot}}, c.history[solanaKey(address)]...)
	if got := read(); got.ForeignRaw != 3 || db.checkpoints[address].ForeignRaw != 1 {
		t.Fatal("confirmed gift persisted before finality")
	}
	delete(c.executions, y.Transaction.Signatures[0])
	c.history[solanaKey(address)] = c.history[solanaKey(address)][1:]
	if got := read(); got.ForeignRaw != 1 {
		t.Fatal("rolled-back gift survived restart")
	}
	c.history[solanaKey(address)] = nil
	if _, err := replayForeignInflows(ctx, db, c, rwa, address, c.finalized+10, c.finalized, ""); err == nil {
		t.Fatal("missing history accepted")
	}
}

func TestForeignInflowRejectsGapAndUnavailableReceipt(t *testing.T) {
	address := rwa.idleATA
	x := foreignFixture(address, 19, 1)
	sig := x.Transaction.Signatures[0]
	anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
	db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{address: anchor}}
	c := &foreignChainFixture{finalized: x.Slot, executions: map[solana.Signature]chain.Execution{}, history: map[solana.PublicKey][]chain.Signed{solanaKey(address): {{Signature: sig, Slot: x.Slot}, {Signature: solana.MustSignatureFromBase58(anchor.Signature), Slot: anchor.Slot}}}}
	if _, err := replayForeignInflows(context.Background(), db, c, rwa, address, x.Slot, x.Slot, ""); !errors.Is(err, chain.ErrNotFound) {
		t.Fatal(err)
	}
	c.executions[sig] = x
	_, err := replayForeignInflows(context.Background(), db, c, rwa, address, x.Slot, x.Slot, "")
	var fault foreignAccountingFault
	if !errors.As(err, &fault) || db.checkpoints[address] != anchor {
		t.Fatalf("gap credited: %v", err)
	}
}

func capturedForeignExecution(t *testing.T, name string) chain.Execution {
	t.Helper()
	raw, err := os.ReadFile("testdata/foreign-inflow/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Result struct {
			Slot        uint64
			Meta        rpc.TransactionMeta
			Transaction solana.Transaction
		}
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	r := captured.Result
	x := chain.Execution{Transaction: &r.Transaction, Inner: r.Meta.InnerInstructions, Receipt: chain.Receipt{Slot: r.Slot, Err: r.Meta.Err, Keys: append(append(r.Transaction.Message.AccountKeys, r.Meta.LoadedAddresses.Writable...), r.Meta.LoadedAddresses.ReadOnly...), Pre: map[solana.PublicKey]chain.TokenBalance{}, Post: map[solana.PublicKey]chain.TokenBalance{}}}
	for _, pair := range []struct {
		rows []rpc.TokenBalance
		dest map[solana.PublicKey]chain.TokenBalance
	}{{r.Meta.PreTokenBalances, x.Pre}, {r.Meta.PostTokenBalances, x.Post}} {
		for _, b := range pair.rows {
			amount, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			pair.dest[x.Keys[b.AccountIndex]] = chain.TokenBalance{Mint: b.Mint, Owner: *b.Owner, Program: *b.ProgramId, Amount: amount, Decimals: b.UiTokenAmount.Decimals}
		}
	}
	return x
}

func TestForeignInflowAuditedAnchor(t *testing.T) {
	x := capturedForeignExecution(t, "anchor")
	if x.Slot != foreignInflowAnchorSlot || x.Transaction.Signatures[0].String() != foreignInflowAnchor {
		t.Fatal("wrong anchor")
	}
	for _, a := range []string{rwa.idleATA, rwa.strategyATA} {
		if _, err := foreignAccountTransfers(rwa, a, x); err != nil {
			t.Fatalf("audited anchor %s: %v", a, err)
		}
	}
	if x.Post[solanaKey(rwa.idleATA)].Amount != 3_633_480_291 || x.Post[solanaKey(rwa.strategyATA)].Amount != 0 {
		t.Fatal("anchor balances changed")
	}
	donation := capturedForeignExecution(t, "donation")
	before := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: x.Slot, BalanceRaw: x.Post[solanaKey(rwa.idleATA)].Amount}
	e, err := advanceForeignCheckpoint(rwa, rwa.idleATA, donation.Transaction.Signatures[0].String(), before, donation)
	if err != nil || e.After.ForeignRaw != 1_000_000 || e.After.BalanceRaw != 3_634_480_291 {
		t.Fatalf("incident replay: %+v %v", e.After, err)
	}
}

func TestForeignObservationEnablesProtectionAndLatchesUnexplainedChanges(t *testing.T) {
	s := monitorSnapshot(t, nil)
	s.Slot = foreignInflowAnchorSlot + 1
	idle := foreignFixture(rwa.idleATA, uint64(s.VoltrIdleRaw), 1)
	custody := foreignFixture(rwa.strategyATA, 0, 1)
	custody.Transaction.Signatures[0] = solana.Signature{2}
	idleSig, custodySig := idle.Transaction.Signatures[0], custody.Transaction.Signatures[0]
	anchorSig := solana.MustSignatureFromBase58(foreignInflowAnchor)
	db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{rwa.idleATA: {Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: uint64(s.VoltrIdleRaw)}, rwa.strategyATA: {Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot}}}
	c := &foreignChainFixture{finalized: foreignInflowAnchorSlot, executions: map[solana.Signature]chain.Execution{idleSig: idle, custodySig: custody}, history: map[solana.PublicKey][]chain.Signed{
		solanaKey(rwa.idleATA):     {{Signature: idleSig, Slot: idle.Slot}, {Signature: anchorSig, Slot: foreignInflowAnchorSlot}},
		solanaKey(rwa.strategyATA): {{Signature: custodySig, Slot: custody.Slot}, {Signature: anchorSig, Slot: foreignInflowAnchorSlot}},
	}}
	s.VoltrIdleRaw++
	s.VoltrStrategyIdleRaw = 1
	if err := observeForeignInflows(context.Background(), db, c, rwa, &s); err != nil {
		t.Fatal(err)
	}
	if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 1 {
		t.Fatalf("proven gifts blocked restore: %+v", got)
	}
	s.HasPosition = true
	s.PositionCollateralRaw, s.PositionDebtRaw = 100, 70
	s.LiquidationThresholdBPS, s.MaxLTVBPS, s.LTVBPS = 7500, 7000, 6500
	if got := Decide(s); got.Action != DeleverRouteStep {
		t.Fatalf("gift blocked hard-LTV protection: %+v", got)
	}
	b := idle.Post[solanaKey(rwa.idleATA)]
	b.Amount++
	idle.Post[solanaKey(rwa.idleATA)] = b
	c.executions[idleSig] = idle
	if err := observeForeignInflows(context.Background(), db, c, rwa, &s); err != nil {
		t.Fatal(err)
	}
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "foreign_inflow_unexplained" {
		t.Fatalf("unexplained surplus did not latch: %+v", got)
	}
}

func TestForeignHistoryCatchupResumesWithoutDoubleCredit(t *testing.T) {
	address := rwa.idleATA
	anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
	db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{address: anchor}}
	c := &foreignChainFixture{finalized: foreignInflowAnchorSlot + 1, executions: map[solana.Signature]chain.Execution{}, history: map[solana.PublicKey][]chain.Signed{solanaKey(address): {{Signature: solana.MustSignatureFromBase58(foreignInflowAnchor), Slot: foreignInflowAnchorSlot}}}}
	for i := 0; i < 70; i++ {
		x := foreignFixture(address, uint64(17+i), 1)
		sig := solana.Signature{byte(i + 1)}
		x.Transaction.Signatures[0] = sig
		c.executions[sig] = x
		c.history[solanaKey(address)] = append([]chain.Signed{{Signature: sig, Slot: x.Slot}}, c.history[solanaKey(address)]...)
	}
	if _, err := replayForeignInflows(context.Background(), db, c, rwa, address, c.finalized, c.finalized, ""); err == nil || db.checkpoints[address].ForeignRaw != 64 {
		t.Fatal("catch-up did not stop at a durable bounded prefix")
	}
	got, err := replayForeignInflows(context.Background(), db, c, rwa, address, c.finalized, c.finalized, "")
	if err != nil || got.ForeignRaw != 70 || got.BalanceRaw != 87 {
		t.Fatalf("same-slot resume: %+v %v", got, err)
	}
}
