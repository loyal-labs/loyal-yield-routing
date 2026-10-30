package backyard

import (
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func foreignFixture(address string, before, gift uint64) chain.Execution {
	owner := rwa.idleAuth
	if address == rwa.strategyATA {
		owner = rwa.strategyAuth
	}
	source := solana.PublicKey{1}
	external := solana.PublicKey{2}
	token := func(authority solana.PublicKey, amount uint64) chain.TokenBalance {
		return chain.TokenBalance{Mint: solanaKey(fleet.USDCMint), Owner: authority, Program: solana.TokenProgramID, Decimals: 6, Amount: amount}
	}
	data := make([]byte, 9)
	data[0] = 3
	binary.LittleEndian.PutUint64(data[1:], gift)
	return chain.Execution{Receipt: chain.Receipt{Slot: foreignInflowAnchorSlot + 1, Keys: []solana.PublicKey{source, solanaKey(address), external, solana.TokenProgramID, voltr.ProgramID},
		Pre:  map[solana.PublicKey]chain.TokenBalance{source: token(external, gift), solanaKey(address): token(solanaKey(owner), before)},
		Post: map[solana.PublicKey]chain.TokenBalance{source: token(external, 0), solanaKey(address): token(solanaKey(owner), before+gift)}},
		Transaction: &solana.Transaction{Signatures: []solana.Signature{{1}}, Message: solana.Message{Instructions: []solana.CompiledInstruction{{ProgramIDIndex: 3, Accounts: []uint16{0, 1, 2}, Data: data}}}}}
}

func TestForeignInflowsRequireExactTransferEvidence(t *testing.T) {
	x := foreignFixture(rwa.idleATA, 17, 1_000_000)
	before := foreignCheckpoint{Signature: "anchor", Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
	e, err := advanceForeignCheckpoint(rwa, rwa.idleATA, x.Transaction.Signatures[0].String(), before, x)
	if err != nil || e.After.ForeignRaw != 1_000_000 {
		t.Fatalf("gift: %+v %v", e.After, err)
	}
	for _, mutate := range []func(*chain.Execution){
		func(x *chain.Execution) {
			b := x.Post[solanaKey(rwa.idleATA)]
			b.Amount++
			x.Post[solanaKey(rwa.idleATA)] = b
		},
		func(x *chain.Execution) {
			b := x.Post[solanaKey(rwa.idleATA)]
			b.Owner = solana.PublicKey{8}
			x.Post[solanaKey(rwa.idleATA)] = b
		},
		func(x *chain.Execution) { b := x.Pre[x.Keys[0]]; b.Owner = solanaKey(rwa.squads); x.Pre[x.Keys[0]] = b },
		func(x *chain.Execution) { x.Transaction.Message.Instructions[0].Data = nil },
		func(x *chain.Execution) { x.Transaction.Message.Instructions[0].Accounts[1] = 99 },
	} {
		x := foreignFixture(rwa.idleATA, 17, 1)
		mutate(&x)
		if _, err := advanceForeignCheckpoint(rwa, rwa.idleATA, x.Transaction.Signatures[0].String(), before, x); err == nil {
			t.Fatal("unexplained gift accepted")
		}
	}
}

func TestForeignInflowsDistinguishVoltrCPIAndMixedTransaction(t *testing.T) {
	x := foreignFixture(rwa.idleATA, 17, 3)
	direct := x.Transaction.Message.Instructions[0]
	protocol := append([]byte(nil), direct.Data...)
	binary.LittleEndian.PutUint64(protocol[1:], 2)
	binary.LittleEndian.PutUint64(direct.Data[1:], 1)
	x.Transaction.Message.Instructions = []solana.CompiledInstruction{{ProgramIDIndex: 4}, direct}
	x.Inner = []rpc.InnerInstruction{{Index: 0, Instructions: []rpc.CompiledInstruction{{ProgramIDIndex: 3, Accounts: direct.Accounts, Data: protocol, StackHeight: 2}}}}
	e, err := advanceForeignCheckpoint(rwa, rwa.idleATA, x.Transaction.Signatures[0].String(), foreignCheckpoint{Slot: foreignInflowAnchorSlot, BalanceRaw: 17}, x)
	if err != nil || e.After.ForeignRaw != 1 {
		t.Fatalf("mixed Voltr deposit and gift: %+v %v", e.After, err)
	}
	x.Inner[0].Instructions[0].StackHeight = 0
	if _, err := foreignAccountTransfers(rwa, rwa.idleATA, x); err == nil {
		t.Fatal("missing ancestry accepted")
	}
}

func TestForeignCustodyCanRestoreButCannotHideMissingStagedFunds(t *testing.T) {
	s := monitorSnapshot(t, nil)
	s.ForeignCustodyRaw, s.VoltrStrategyIdleRaw = 1, 1
	if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 1 {
		t.Fatalf("gift blocked: %+v", got)
	}
	s.StageTransient, s.StagedAmountKnown, s.StagedAmountRaw = true, true, 7
	s.VoltrStrategyIdleRaw = 8
	if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 8 {
		t.Fatalf("staged+gift blocked: %+v", got)
	}
	s.VoltrStrategyIdleRaw--
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatalf("missing principal accepted: %+v", got)
	}
}

func TestForeignInflowIgnoresHarmlessAccountMentions(t *testing.T) {
	x := foreignFixture(rwa.idleATA, 17, 0)
	// Approve on somebody else's account can carry idle as an unused extra meta.
	x.Transaction.Message.Instructions = append(x.Transaction.Message.Instructions,
		solana.CompiledInstruction{ProgramIDIndex: 3, Accounts: []uint16{0, 2, 2, 1}, Data: append([]byte{4}, make([]byte, 8)...)},
		solana.CompiledInstruction{ProgramIDIndex: 3, Accounts: []uint16{1}, Data: []byte{22}})
	delete(x.Pre, x.Keys[0])
	delete(x.Post, x.Keys[0])
	e, err := advanceForeignCheckpoint(rwa, rwa.idleATA, x.Transaction.Signatures[0].String(), foreignCheckpoint{Slot: foreignInflowAnchorSlot, BalanceRaw: 17}, x)
	if err != nil || e.After.ForeignRaw != 0 {
		t.Fatalf("permissionless no-op halted route: %+v %v", e.After, err)
	}
	x.Transaction.Message.Instructions = append(x.Transaction.Message.Instructions, solana.CompiledInstruction{ProgramIDIndex: 3, Accounts: []uint16{1, 2, 2}, Data: append([]byte{4}, make([]byte, 8)...)})
	if _, err := foreignAccountTransfers(rwa, rwa.idleATA, x); err == nil {
		t.Fatal("approval changing custody authority ignored")
	}
}

func TestForeignInflowFromAccountCreatedAndClosedInSameTransaction(t *testing.T) {
	x := foreignFixture(rwa.idleATA, 17, 1)
	delete(x.Pre, x.Keys[0])
	delete(x.Post, x.Keys[0])
	x.Keys = append(x.Keys, solanaKey(fleet.USDCMint))
	initialize := solana.CompiledInstruction{ProgramIDIndex: 3, Accounts: []uint16{0, 5}, Data: append([]byte{18}, x.Keys[2][:]...)}
	x.Transaction.Message.Instructions = append([]solana.CompiledInstruction{initialize}, x.Transaction.Message.Instructions...)
	e, err := advanceForeignCheckpoint(rwa, rwa.idleATA, x.Transaction.Signatures[0].String(), foreignCheckpoint{Slot: foreignInflowAnchorSlot, BalanceRaw: 17}, x)
	if err != nil || e.After.ForeignRaw != 1 {
		t.Fatalf("temporary source gift: %+v %v", e.After, err)
	}
	x.Transaction.Message.Instructions[0].Data = append([]byte{18}, solanaKey(rwa.squads).Bytes()...)
	if _, err := foreignAccountTransfers(rwa, rwa.idleATA, x); err == nil {
		t.Fatal("temporary owned custody counted as foreign")
	}
	x = foreignFixture(rwa.idleATA, 17, 1)
	changeOwner := solana.CompiledInstruction{ProgramIDIndex: 3, Accounts: []uint16{0, 2}, Data: append([]byte{6, 2, 1}, solanaKey(rwa.squads).Bytes()...)}
	x.Transaction.Message.Instructions = append([]solana.CompiledInstruction{changeOwner}, x.Transaction.Message.Instructions...)
	if _, err := foreignAccountTransfers(rwa, rwa.idleATA, x); err == nil {
		t.Fatal("source ownership change counted owned custody as foreign")
	}
}
