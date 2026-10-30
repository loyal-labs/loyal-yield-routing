package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func foreignSweepFixture(amount uint64) chain.Execution {
	x := foreignFixture(rwa.strategyATA, amount, 0)
	x.Slot += 2
	x.Transaction.Signatures[0] = solana.Signature{3}
	x.Keys = []solana.PublicKey{solanaKey(rwa.strategyATA), solanaKey(rwa.idleATA), solanaKey(rwa.strategyAuth), solana.TokenProgramID, voltr.ProgramID}
	token := func(owner string, amount uint64) chain.TokenBalance {
		return chain.TokenBalance{Mint: solanaKey(fleet.USDCMint), Owner: solanaKey(owner), Program: solana.TokenProgramID, Decimals: 6, Amount: amount}
	}
	x.Pre = map[solana.PublicKey]chain.TokenBalance{x.Keys[0]: token(rwa.strategyAuth, amount), x.Keys[1]: token(rwa.idleAuth, 10), solanaKey(rwa.cash): token(rwa.squads, 0)}
	x.Post = map[solana.PublicKey]chain.TokenBalance{x.Keys[0]: token(rwa.strategyAuth, 0), x.Keys[1]: token(rwa.idleAuth, 10+amount), solanaKey(rwa.cash): token(rwa.squads, 0)}
	data := make([]byte, 9)
	data[0] = 3
	binary.LittleEndian.PutUint64(data[1:], amount)
	x.Transaction.Message.Instructions = []solana.CompiledInstruction{{ProgramIDIndex: 4}}
	x.Inner = []rpc.InnerInstruction{{Index: 0, Instructions: []rpc.CompiledInstruction{{ProgramIDIndex: 3, Accounts: []uint16{0, 1, 2}, Data: data, StackHeight: 2}}}}
	return x
}

func TestForeignInflowDuringSignedRestoreReconcilesExactly(t *testing.T) {
	ctx := context.Background()
	planned := uint64(7)
	effects, _, _, _, err := bridgeExpectedEffects(rwa, Decision{Action: VoltrRestoreIdle, AmountRaw: int64(planned)}, 10, planned, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind = effectsBridge
	foreign := uint64(0)
	effects.ForeignCustodyRaw = &foreign
	donation := foreignFixture(rwa.strategyATA, planned, 1)
	sweep := foreignSweepFixture(planned + 1)
	donationSig, sweepSig := donation.Transaction.Signatures[0], sweep.Transaction.Signatures[0]
	anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: planned}
	db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{rwa.strategyATA: anchor}}
	c := &foreignChainFixture{finalized: sweep.Slot, executions: map[solana.Signature]chain.Execution{donationSig: donation, sweepSig: sweep}, history: map[solana.PublicKey][]chain.Signed{solanaKey(rwa.strategyATA): {{Signature: sweepSig, Slot: sweep.Slot}, {Signature: donationSig, Slot: donation.Slot}, {Signature: solana.MustSignatureFromBase58(anchor.Signature), Slot: anchor.Slot}}}}
	pre, _ := transactionTokenBalances(sweep.Pre)
	post, _ := transactionTokenBalances(sweep.Post)
	receipt := ConfirmedTransactionEvidence{Signature: sweepSig.String(), Slot: int64(sweep.Slot), Finalized: true, PreTokenBalances: pre, PostTokenBalances: post}
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err == nil {
		t.Fatal("unproven surplus was accepted")
	}
	if err := proveForeignSweep(ctx, db, c, rwa, effects, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.foreignSweepExtraRaw != 1 {
		t.Fatal("wrong foreign allowance")
	}
	_, body, err := ReconcileConfirmedTransaction(rwa, effects, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"foreignSweepExtraRaw":1`)) {
		t.Fatal("reconciliation lost gift provenance")
	}
	// Restart before MarkReconciled must reproduce the same proof without crediting twice.
	if err := proveForeignSweep(ctx, db, c, rwa, effects, &receipt); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := replayForeignInflows(ctx, db, c, rwa, rwa.strategyATA, sweep.Slot, sweep.Slot, "")
	if err != nil || checkpoint.ForeignRaw != 0 || checkpoint.BalanceRaw != 0 {
		t.Fatalf("swept credit survived: %+v %v", checkpoint, err)
	}
	receipt.PostTokenBalances[0].Raw++
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err == nil {
		t.Fatal("proof hid an unrelated custody change")
	}
}

func TestForeignSweepDoesNotRaisePrincipalCapAndWireStillPersists(t *testing.T) {
	foreign := uint64(1)
	total := strategyTwoBridgeLegCapRaw + foreign
	effects, _, _, _, err := bridgeExpectedEffects(rwa, Decision{Action: VoltrRestoreIdle, AmountRaw: int64(total)}, 10, total, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind = effectsBridge
	effects.ForeignCustodyRaw = &foreign
	encoded, _ := json.Marshal(effects)
	if _, err := DecodeExpectedEffects(rwa, encoded); err != nil {
		t.Fatal(err)
	}
	foreign = 0
	if err := validateForeignSweep(rwa, effects); err == nil {
		t.Fatal("principal above cap accepted")
	}
	foreign = 1
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	m := readyWorkerManifest(t)
	accounts := append(routeNAVFixture(t, 77), exactReportTicketAccount(t, 4))
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], total)
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{Slot: 77, RouteLane: PrimeUSDCLaneID, StrategyKey: PrimeUSDCLaneID, VoltrIdleRaw: 11, VoltrStrategyIdleRaw: int64(total), SquadsIdleRaw: 6, ForeignCustodyRaw: 1}, policies: testPolicies(t), planning: &routePlanningState{inputs: targetInputs{ExitCosts: fixtureExitCosts()}}}
	client := newFakeChain(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":78},"value":{"blockhash":%q,"lastValidBlockHeight":999}}}`, bridgeVault)), nil
	}))
	_, built, err := prepareBridgeFromObservedAccounts(context.Background(), client, m, Decision{Action: VoltrRestoreIdle, AmountRaw: int64(total), StrategyKey: PrimeUSDCLaneID}, o, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if built.Request.AmountRaw != strategyTwoBridgeLegCapRaw || built.ExpectedEffects.ForeignCustodyRaw == nil || *built.ExpectedEffects.ForeignCustodyRaw != 1 {
		t.Fatal("production preparation lost principal/gift binding")
	}
	signed, err := buildAndSignBridgeTransactionForDelegate(built.Request, key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signed.BuildResult(123)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatal(err)
	}
}
