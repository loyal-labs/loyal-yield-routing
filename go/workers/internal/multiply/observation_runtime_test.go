package multiply

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type bankObservationReader struct {
	accounts map[string]*chain.Account
	slot     uint64
	batches  int
}

func (r *bankObservationReader) Accounts(_ context.Context, keys []solana.PublicKey, _ rpc.CommitmentType, _ uint64) (uint64, []*chain.Account, error) {
	r.batches++
	accounts := make([]*chain.Account, len(keys))
	for i, key := range keys {
		accounts[i] = r.accounts[key.String()]
	}
	return r.slot, accounts, nil
}

// behindReader is a node that has not reached the slot it is asked for.
type behindReader struct{ asked *uint64 }

func (r behindReader) Accounts(_ context.Context, _ []solana.PublicKey, _ rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	*r.asked = minContextSlot
	return 0, nil, fmt.Errorf("getMultipleAccounts: %w", chain.ErrBehind)
}

func TestReconciliationWaitsForANodeAtTheConfirmedSlot(t *testing.T) {
	// A lagging or unavailable node says nothing about the confirmed bank, so
	// reconciliation waits instead of moving the route to manual recovery.
	var asked uint64
	_, err := ObserveConfirmed(context.Background(), reconciliationReader{behindReader{&asked}, 1000}, testTopology(t), nil)
	if asked != 1000 || !errors.Is(err, errReconciliationBankUnavailable) {
		t.Fatalf("reconciliation read below the receipt slot or became a verdict: asked %d, %v", asked, err)
	}
}

func TestReceiptReadWaitsOnlyForAbsenceOrUnavailability(t *testing.T) {
	executor, fake, _ := testExecutor(t)
	signed := signedWireFixture(t, false)
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	op := &MultiplyOperation{SignedWire: signed.Wire, SignedWireSHA256: &signed.WireSHA256, TransactionSignature: &signed.TransactionSignature, RecentBlockhash: &signed.RecentBlockhash, MessageSHA256: &messageHash}
	for _, test := range []struct {
		err   error
		waits bool
	}{
		{chain.ErrNotFound, true},
		{fmt.Errorf("getTransaction: %w", chain.ErrBehind), true},
		{errors.New("getTransaction: invalid token amount"), false},
	} {
		fake.receiptErr = test.err
		_, err := executor.readReceipt(context.Background(), op, testTopology(t))
		if err == nil || errors.Is(err, errReceiptUnavailable) != test.waits {
			t.Fatalf("receipt error %v: waits=%v, got %v", test.err, test.waits, err)
		}
	}
}

func observedToken(key solana.PublicKey, mint string, owner solana.PublicKey, amount uint64) *chain.Account {
	data := make([]byte, 165)
	mintKey := mustKey(mint)
	copy(data[:32], mintKey[:])
	copy(data[32:64], owner[:])
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = 1
	return &chain.Account{Key: key, Owner: mustKey(TokenProgram), Lamports: 1, Data: data}
}

func emptyBankReader(topology *EarnMaxTopology) *bankObservationReader {
	reader := &bankObservationReader{accounts: map[string]*chain.Account{}, slot: 500}
	reader.accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 4_000_000)
	for _, config := range topology.StrategyCatalog() {
		reader.accounts[config.CollateralReserve.String()] = reviewReserveAccount(config, false)
		reader.accounts[config.DebtReserve.String()] = reviewReserveAccount(config, true)
	}
	return reader
}

func TestReceiptAndRouteCustodyShareOneBankSnapshot(t *testing.T) {
	topology := testTopology(t)
	reader := emptyBankReader(topology)
	destination := fixtureKey(211)
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(210), 2_000_000)
	extra := []TokenBalance{{Account: destination.String(), Mint: USDCMint, TokenProgram: TokenProgram}}
	observed, err := ObserveConfirmed(context.Background(), reader, topology, extra)
	if err != nil {
		t.Fatal(err)
	}
	if reader.batches != 1 || observed.Slot != 500 || observed.Claim.AmountRaw != 4_000_000 || observed.ExternalCustody[0].AmountRaw != 2_000_000 {
		t.Fatal("financial receipt was not bound to a single bank snapshot")
	}
	// Nil entries supplied by a complete RPC snapshot are known absent, not
	// transport omissions; undeployed obligations contain no debt or principal.
	for _, position := range observed.Strategies {
		if position == nil || position.DebtRaw != 0 || position.CollateralDepositedRaw != 0 {
			t.Fatal("known absent obligation fabricated exposure")
		}
	}
	delete(reader.accounts, destination.String())
	if _, err := ObserveConfirmed(context.Background(), reader, topology, extra); err == nil {
		t.Fatal("missing receipt destination became a zero-balance proof")
	}
}

func TestObservationRefusesFutureAccountAndUnknownPlannerCustody(t *testing.T) {
	topology := testTopology(t)
	reader := emptyBankReader(topology)
	config := topology.Strategies[SyrupUsdcUsdc]
	binary.LittleEndian.PutUint64(reader.accounts[config.CollateralReserve.String()].Data[16:24], 501)
	if _, err := ObserveConfirmed(context.Background(), reader, topology, nil); err == nil {
		t.Fatal("future reserve escaped bank-slot validation")
	}
	observed := idleObserved(topology)
	observed.DebtCustodies = nil
	route := testRouteState(t, topology)
	route.Goal = GoalWithdraw
	if decision := NextAction(route, observed, topology); decision.Kind != "invalid_observation" {
		t.Fatal("missing custody completed withdrawal")
	}
	operationID := "immutable-attempt"
	route.CurrentOperationID = &operationID
	if decision := NextAction(route, nil, topology); decision.Kind != "resume" {
		t.Fatal("unknown fresh observation hid an already owned recovery")
	}
	if decision := NextAction(route, observed, topology); decision.Kind != "resume" {
		t.Fatal("missing fresh admission evidence hid owned recovery")
	}
}

func TestWithdrawalExactPayoutRequiresEnoughConfirmedLiquidity(t *testing.T) {
	// Exact-payout semantics are independently established by the App's
	// persisted request and wallet-signed transfer_checked, not planner defaults.
	topology := testTopology(t)
	destination := fixtureKey(212).String()
	for _, test := range []struct {
		name  string
		claim uint64
		want  string
	}{
		{"fee_loss", 9_990_000, "withdrawal_liquidity_shortfall"},
		{"nav_loss", 8_000_000, "withdrawal_liquidity_shortfall"},
		{"exact", 10_000_000, "withdrawal_claimable"},
		{"surplus", 11_000_000, "withdrawal_claimable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := testRouteState(t, topology)
			route.Goal = GoalWithdraw
			now := time.Now().UTC()
			route.Withdrawal = &Withdrawal{RequestID: "exact-user-request", DestinationAccount: destination, AmountRaw: 10_000_000, Status: WithdrawalRequested, RequestedAt: now, ReadyBy: now.Add(10 * time.Minute)}
			observed := idleObserved(topology)
			observed.Claim.AmountRaw = test.claim
			observed.ExternalCustody = []TokenBalance{{Account: destination, Mint: USDCMint, TokenProgram: TokenProgram}}
			condition, err := withdrawalClaimCondition(route, observed, topology)
			if err != nil || condition != test.want {
				t.Fatalf("condition %s %v", condition, err)
			}
			if route.Withdrawal.AmountRaw != 10_000_000 || route.Withdrawal.Status != WithdrawalRequested {
				t.Fatal("eligibility rewrote exact user intent")
			}
			serialized, err := jsonMarshal(route)
			if err != nil {
				t.Fatal(err)
			}
			var restarted RouteState
			if err := jsonUnmarshalStrict(serialized, &restarted); err != nil {
				t.Fatal(err)
			}
			if condition, err := withdrawalClaimCondition(&restarted, observed, topology); err != nil || condition != test.want {
				t.Fatalf("restart changed payout evidence: %s %v", condition, err)
			}
			observed.ExternalCustody = nil
			if _, err := withdrawalClaimCondition(route, observed, topology); err == nil {
				t.Fatal("missing destination became claimable")
			}
			if _, err := withdrawalClaimCondition(route, nil, topology); err == nil {
				t.Fatal("missing latest snapshot became claimable")
			}
		})
	}
}
