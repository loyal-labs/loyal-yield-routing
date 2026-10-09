package multiply

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// Publish an actual SDK-constrained, simulated wire using the same durable
// publication APIs, then stop before intent. No transaction or poststate is
// fabricated: the following keyless Tick must execute/reconcile it itself.
func (f *multiplySVMFixture) persistUnsentDeposit(t *testing.T) *MultiplyOperation {
	t.Helper()
	before, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := NextAction(f.state, before, f.topology).Plan
	if plan == nil || plan.Action != ActionDepositCollateral {
		t.Fatal("initial actual bank has no deposit plan")
	}
	built, err := BuildOperation(plan, before, f.topology, fakeQuoteClient{f.topology}, f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindBefore(&built.ExpectedEffects, before, plan.StrategyKey, f.topology); err != nil {
		t.Fatal(err)
	}
	policy, err := f.executor.EnsureExactPolicy(f.ctx, f.topology, plan, built)
	if err != nil {
		t.Fatal(err)
	}
	signed, slot, err := f.executor.PrepareAndSign(f.ctx, built, policy.Account, 0, policy.ConstraintIndexes, before.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.Simulate(f.ctx, signed, slot); err != nil {
		t.Fatalf("actual prepublication simulation: %v", err)
	}
	lease, err := f.store.LeaseRoute(f.ctx, f.state.RouteKey, "actual-prepublication-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer f.store.ReleaseLease(f.ctx, lease)
	now := time.Now().UTC()
	id := OperationID(f.state.RouteKey, f.state.Cycle, f.state.Generation, string(plan.Action))
	op := &MultiplyOperation{OperationID: id, RouteKey: f.state.RouteKey, Cycle: f.state.Cycle, EngineVersion: EngineVersion, Action: plan.Action, StrategyKey: plan.StrategyKey, Status: StatusPrepared, IDempotencyKey: fmt.Sprintf("%s:%s:%d:%d", EngineVersion, f.state.RouteKey, f.state.Cycle, f.state.Generation), ExpectedEffects: built.ExpectedEffects, CreatedAt: now, UpdatedAt: now}
	next := *f.state
	next.Generation++
	next.CurrentOperationID = &id
	if ok, err := f.store.PrepareOperation(f.ctx, lease, &next, op); err != nil || !ok {
		t.Fatalf("prepare: %v %v", ok, err)
	}
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.PersistSignedOperation(f.ctx, lease, id, policy.Account.String(), policy.DataSHA256, messageHash, signed); err != nil || !ok {
		t.Fatalf("actual signed publication: %v %v", ok, err)
	}
	op, err = f.store.LoadOperation(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestCurrentGoMultiplyUnsentWireKeylessRecoveryWithoutPolicyReadiness(t *testing.T) {
	f := newMultiplySVMFixture(t)
	original := f.persistUnsentDeposit(t)
	if _, err := f.store.Pool().Exec(f.ctx, `DELETE FROM loyal_yield.earn_max_policy_sets WHERE settings=$1`, f.state.Settings); err != nil {
		t.Fatal(err)
	}
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "operation_reconciled" {
		t.Fatalf("actual immutable first send: %v %v", result, err)
	}
	saved, err := f.store.LoadOperation(f.ctx, original.OperationID)
	if err != nil || saved.TransactionSignature == nil || *saved.TransactionSignature != *original.TransactionSignature || *saved.SignedWireSHA256 != *original.SignedWireSHA256 {
		t.Fatal("keyless first send changed identity")
	}
	f.assertReconciled(t, 1)
}

func TestCurrentGoMultiplyActualExpiredUnsentWireNoEffect(t *testing.T) {
	f := newMultiplySVMFixture(t)
	original := f.persistUnsentDeposit(t)
	if _, err := f.bank.call("advanceSlot", []any{*original.LastValidBlockHeight + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.call("expireBlockhash", []any{}); err != nil {
		t.Fatal(err)
	}
	var failed *chain.SimulationError
	if _, err := f.executor.Simulate(f.ctx, &SignedOperation{Wire: original.SignedWire}, *original.LastValidBlockHeight+1); !errors.As(err, &failed) {
		t.Fatalf("obsolete actual bank wire still valid: %v", err)
	}
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "operation_expired_without_effect" {
		t.Fatalf("actual bank absence proof: %v %v", result, err)
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation != nil {
		t.Fatal("proved expiry retained current operation")
	}
	var status string
	var wireCleared bool
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT status,signed_wire IS NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, original.OperationID).Scan(&status, &wireCleared); err != nil || status != "expired" || !wireCleared {
		t.Fatalf("expiry must match Rust expire_multiply_operation: %s %v %v", status, wireCleared, err)
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 1_000_000 || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 {
		t.Fatal("expiry changed financial state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 0 {
		t.Fatal("expired recovery sent obsolete packet")
	}
}

func TestCurrentGoMultiplyActualDepositThenWithdrawal(t *testing.T) {
	f := newMultiplySVMFixture(t)
	if result, err := f.worker(t, false).Tick(f.ctx); err != nil || result.Condition != "operation_reconciled" {
		t.Fatalf("deposit: %v %v", result, err)
	}
	lease, err := f.store.LeaseRoute(f.ctx, f.state.RouteKey, "actual-withdraw-request", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saved.State.RequestWithdrawal("actual-root-request", fixtureKey(120).String(), 1_000_000, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.SaveRouteState(f.ctx, lease, saved.State); err != nil || !ok {
		t.Fatalf("saved request: %v %v", ok, err)
	}
	if _, err := f.store.ReleaseLease(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
	result, err := f.worker(t, false).Tick(f.ctx)
	if err != nil || result.Condition != "operation_reconciled" {
		t.Fatalf("actual unwind: %v %v", result, err)
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 1_000_000 {
		t.Fatal("actual withdrawal financial state differs from receipt")
	}
	saved, err = f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation != nil || saved.State.Withdrawal.Status != WithdrawalRequested || saved.State.Withdrawal.AmountRaw != 1_000_000 || saved.State.Withdrawal.DestinationAccount != fixtureKey(120).String() {
		t.Fatal("non-USDC unwind fabricated payout completion or changed request")
	}
	var receipts int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operations o WHERE o.route_key=$1 AND o.status='reconciled'`, f.state.RouteKey).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatalf("deposit and unwind receipts: %d %v", receipts, err)
	}
}

func TestCurrentGoMultiplyActualReceiptTerminalRollsBackOnStaleFence(t *testing.T) {
	f := newMultiplySVMFixture(t)
	saved := f.landLostResponse(t)
	lease, err := f.store.LeaseRoute(f.ctx, f.state.RouteKey, "actual-receipt-stale-proof", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if ok, err := f.store.MarkConfirmed(f.ctx, lease, saved.Operation.OperationID, 1000); err != nil || !ok {
		t.Fatalf("confirmed actual bank: %v %v", ok, err)
	}
	op, err := f.store.LoadOperation(f.ctx, saved.Operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.executor.readReceipt(f.ctx, op, f.topology)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExpectedEffects(&op.ExpectedEffects, op.Action, nil, after, f.topology); err != nil {
		t.Fatal(err)
	}
	proof.evidence.ObservationSlot = after.Slot
	next := *saved.State
	next.Generation++
	next.CurrentOperationID = nil
	next.ObservedSlot = after.Slot
	stale := *lease
	stale.FencingToken++
	if ok, err := f.store.ReconcileOperation(f.ctx, &stale, op.OperationID, *op.TransactionSignature, PolicyDataHash([]byte("actual-receipt")), 1000, &next, proof); err != nil || ok {
		t.Fatalf("stale financial CAS: %v %v", ok, err)
	}
	var n int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='reconciled'`, op.OperationID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rolled back terminal leaked immutable receipt: %d %v", n, err)
	}
	held, err := f.store.LoadOperation(f.ctx, op.OperationID)
	if err != nil || held.Status != StatusConfirmed || !bytes.Equal(held.SignedWire, op.SignedWire) {
		t.Fatal("stale terminal changed actual financial ownership")
	}
	if _, err := f.store.ReleaseLease(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "confirmed_operation_reconciled" {
		t.Fatalf("confirmed actual receipt recovery: %v %v", result, err)
	}
	f.assertReconciled(t, 1)
}

func TestCurrentGoMultiplyMockRejectsInvalidScopeRefresh(t *testing.T) {
	f := newMultiplySVMFixture(t)
	config := f.topology.Strategies[SyrupUsdcUsdc]
	for _, mutation := range []struct {
		name   string
		change func(*Instruction)
	}{
		{"different-oracle", func(ix *Instruction) { ix.Accounts[5].PubKey = config.MarketAuthority }},
		{"writable-oracle", func(ix *Instruction) { ix.Accounts[5].IsWritable = true }},
		{"signer-oracle", func(ix *Instruction) {
			ix.Accounts[5].PubKey = solana.PublicKey(f.executor.Signer[32:])
			ix.Accounts[5].IsSigner = true
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			before, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan := NextAction(f.state, before, f.topology).Plan
			built, err := BuildOperation(plan, before, f.topology, fakeQuoteClient{f.topology}, f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := f.executor.EnsureExactPolicy(f.ctx, f.topology, plan, built)
			if err != nil {
				t.Fatal(err)
			}
			mutation.change(&built.PreInstructions[0])
			signed, slot, err := f.executor.PrepareAndSign(f.ctx, built, policy.Account, 0, policy.ConstraintIndexes, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.executor.Simulate(f.ctx, signed, slot); err == nil {
				t.Fatal("invalid oracle fixture vector simulated successfully")
			}
		})
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 1_000_000 || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 {
		t.Fatal("invalid simulation changed financial bank")
	}
}

func TestCurrentGoMultiplyActualReceiptRejectsProviderDrift(t *testing.T) {
	f := newMultiplySVMFixture(t)
	saved := f.landLostResponse(t)
	signature := solana.MustSignatureFromBase58(*saved.Operation.TransactionSignature)
	receipt, err := f.cluster.Receipt(f.ctx, signature, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateConfirmedReceipt(saved.Operation, f.topology, receipt, nil); err != nil {
		t.Fatalf("unmodified actual bank receipt: %v", err)
	}
	foreign := func(change func(*chain.TokenBalance)) func(*chain.Receipt) {
		return func(r *chain.Receipt) {
			post := map[solana.PublicKey]chain.TokenBalance{}
			for key, balance := range r.Post {
				change(&balance)
				post[key] = balance
			}
			r.Post = post
		}
	}
	for _, mutation := range []struct {
		name   string
		change func(*chain.Receipt)
	}{
		{"failed-transaction", func(r *chain.Receipt) { r.Err = "failed" }},
		{"unknown-slot", func(r *chain.Receipt) { r.Slot = 0 }},
		{"different-wire", func(r *chain.Receipt) { r.Wire = []byte{0} }},
		{"incomplete-sol", func(r *chain.Receipt) { r.PostLamports = nil }},
		{"different-loaded-order", func(r *chain.Receipt) { r.LoadedWritable = []solana.PublicKey{fixtureKey(122)} }},
		{"missing-token-balance", func(r *chain.Receipt) { r.Post = nil }},
		{"foreign-token-authority", foreign(func(b *chain.TokenBalance) { b.Owner = fixtureKey(123) })},
		{"foreign-token-program", foreign(func(b *chain.TokenBalance) { b.Program = squads.ProgramID })},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := receipt
			mutation.change(&changed)
			if _, err := validateConfirmedReceipt(saved.Operation, f.topology, changed, nil); err == nil {
				t.Fatal("provider drift accepted as actual financial receipt")
			}
		})
	}
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "operation_reconciled" {
		t.Fatalf("original actual receipt no longer recoverable: %v %v", result, err)
	}
	f.assertReconciled(t, 1)
}

// Source-layout initial exposures test refusal only; they are not execution
// receipts or claimed financial poststates. Worker must publish no operation.
func TestCurrentGoMultiplyInitialExposureAdmissionHolds(t *testing.T) {
	for _, test := range []struct {
		name      string
		keys      []StrategyKey
		condition string
	}{
		{"different-active-strategy", []StrategyKey{PrimeUsdc}, "unsupported_deploy_active_strategy"},
		{"multiple-active-strategies", []StrategyKey{PrimeUsdc, SyrupUsdcUsdc}, "awaiting_coherent_confirmed_observation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMultiplySVMFixtureWithInitialAccounts(t, func(topology *EarnMaxTopology, accounts map[string]*chain.Account) {
				for _, key := range test.keys {
					config := topology.Strategies[key]
					data := make([]byte, kamino.ObligationSize)
					copy(data[:8], kamino.ObligationDiscriminator[:])
					binary.LittleEndian.PutUint64(data[16:24], 1000)
					copy(data[32:64], config.Market[:])
					copy(data[64:96], topology.Vault[:])
					copy(data[96:128], config.CollateralReserve[:])
					binary.LittleEndian.PutUint64(data[128:136], 1000)
					accounts[config.Obligation.String()] = &chain.Account{Key: config.Obligation, Owner: kamino.ProgramID, Data: data, Lamports: 10_000_000}
				}
			})
			result, err := f.worker(t, false).Tick(f.ctx)
			if err != nil || result.Condition != test.condition {
				t.Fatalf("initial source bank refusal: %v %v", result, err)
			}
			saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
			if err != nil || saved.Operation != nil || saved.State.Generation != f.state.Generation {
				t.Fatal("unsupported exposure changed durable admission")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 0 {
				t.Fatal("unsupported initial exposure sent financial wire")
			}
		})
	}
}

func TestCurrentGoMultiplyActualLeveredDeployAndFullUnwind(t *testing.T) {
	f := newMultiplySVMFixture(t)
	worker := f.worker(t, false)
	for tick := 0; tick < 80; tick++ {
		f.advanceActualBank(t)
		result, err := worker.Tick(f.ctx)
		if err != nil {
			t.Fatalf("actual deploy tick%d: %v", tick, err)
		}
		if result.Condition == "route_complete" {
			break
		}
		if result.Condition != "operation_reconciled" {
			t.Fatalf("actual deploy tick%d: %v", tick, result)
		}
		if tick == 79 {
			t.Fatal("fixed-price deploy did not converge")
		}
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	position := after.Position(SyrupUsdcUsdc)
	if position.DebtRaw == 0 || position.CollateralDepositedRaw <= 1_000_000 || after.Claim.AmountRaw != 0 || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 0 {
		t.Fatal("actual model never executed the levered loop")
	}
	if position.CollateralDepositedRaw-position.DebtRaw != 1_000_000 {
		t.Fatal("real SPL fixed-price deploy changed equity")
	}
	lease, err := f.store.LeaseRoute(f.ctx, f.state.RouteKey, "actual-full-unwind-request", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saved.State.RequestWithdrawal("actual-full-unwind-root-request", fixtureKey(120).String(), 1_000_000, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.SaveRouteState(f.ctx, lease, saved.State); err != nil || !ok {
		t.Fatalf("save actual withdrawal %v %v", ok, err)
	}
	if _, err := f.store.ReleaseLease(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
	f.advanceActualBank(t)
	duplicateHeld := false
	for tick := 0; tick < 80; tick++ {
		if tick > 3 {
			f.advanceActualBank(t)
		}
		result, err := worker.Tick(f.ctx)
		if err != nil {
			if tick == 3 && strings.Contains(err.Error(), "AlreadyProcessed") {
				held, loadErr := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
				if loadErr != nil || held.Operation == nil || held.Operation.Status != StatusPrepared || len(held.Operation.SignedWire) != 0 || held.Operation.TransactionSignature != nil {
					t.Fatal("duplicate old-hash candidate adopted old signed financial identity")
				}
				var receipts int
				if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='reconciled'`, held.Operation.OperationID).Scan(&receipts); err != nil || receipts != 0 {
					t.Fatal("duplicate unsigned candidate gained financial evidence")
				}
				recovered, err := f.worker(t, true).Tick(f.ctx)
				if err != nil || recovered.Condition != "prepared_operation_rebuilt" {
					t.Fatalf("unsigned duplicate retry/cancel %v %v", recovered, err)
				}
				duplicateHeld = true
				continue
			}
			t.Fatalf("actual unwind tick%d: %v", tick, err)
		}
		saved, err = f.store.LoadRouteState(f.ctx, f.state.RouteKey)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State.Withdrawal.Status == WithdrawalClaimable {
			break
		}
		if result.Condition != "operation_reconciled" {
			t.Fatalf("actual unwind tick%d: %v", tick, result)
		}
		if tick == 79 {
			t.Fatal("fixed-price unwind did not converge")
		}
	}
	if !duplicateHeld {
		t.Fatal("old-hash identical MaxSafe candidate refusal was not exercised")
	}
	after, err = ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.Position(SyrupUsdcUsdc).DebtRaw != 0 || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 0 || after.Claim.AmountRaw != 1_000_000 {
		t.Fatal("actual full unwind changed equity or retained exposure")
	}
	if saved.State.Withdrawal.AmountRaw != 1_000_000 || saved.State.Withdrawal.DestinationAccount != fixtureKey(120).String() || saved.State.Withdrawal.RequestID != "actual-full-unwind-root-request" || saved.State.Withdrawal.ClaimSignature != nil {
		t.Fatal("delegate changed exact root-owned request or claimed without wallet")
	}
	var kinds, missing int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(DISTINCT action),count(*) FILTER(WHERE o.reconciliation_sha256 IS NULL) FROM loyal_yield.multiply_operations o WHERE o.route_key=$1 AND o.status='reconciled'`, f.state.RouteKey).Scan(&kinds, &missing); err != nil || kinds < 6 || missing != 0 {
		t.Fatalf("actual lifecycle recipes/receipts: %d %d %v", kinds, missing, err)
	}
}

func (f *multiplySVMFixture) advanceActualBank(t *testing.T) {
	t.Helper()
	raw, err := f.bank.call("getSlot", []any{})
	if err != nil {
		t.Fatal(err)
	}
	var slot uint64
	if err := json.Unmarshal(raw, &slot); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.call("expireBlockhash", []any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.call("advanceSlot", []any{slot + 1}); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentGoMultiplyActualModelRejectsDebtAndSwapRecipeDrift(t *testing.T) {
	f := newMultiplySVMFixture(t)
	for tick := 0; tick < 1; tick++ {
		result, err := f.worker(t, false).Tick(f.ctx)
		if err != nil || result.Condition != "operation_reconciled" {
			t.Fatalf("actual debt initial execution %v %v", result, err)
		}
	}
	before, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	borrowed := false
	for _, test := range []struct {
		name   string
		plan   ActionPlan
		change func(*Instruction)
	}{
		{"borrow-wrong-source", ActionPlan{Action: ActionBorrowDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(1)}, func(ix *Instruction) {
			ix.Accounts[6].PubKey = f.topology.Strategies[SyrupUsdcUsdc].CollateralLiquiditySupply
		}},
		{"borrow-wrong-fee-vault", ActionPlan{Action: ActionBorrowDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(1)}, func(ix *Instruction) { ix.Accounts[7].PubKey = f.topology.ClaimCustody }},
		{"borrow-wrong-farm", ActionPlan{Action: ActionBorrowDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(1)}, func(ix *Instruction) { ix.Accounts[12].PubKey = f.topology.Strategies[SyrupUsdcUsdc].MarketAuthority }},
		{"borrow-wrong-count", ActionPlan{Action: ActionBorrowDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(1)}, func(ix *Instruction) { ix.Accounts = ix.Accounts[:14] }},
		{"repay-wrong-destination", ActionPlan{Action: ActionRepayDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountAll}, func(ix *Instruction) {
			ix.Accounts[5].PubKey = f.topology.Strategies[SyrupUsdcUsdc].CollateralLiquiditySupply
		}},
		{"repay-wrong-token-program", ActionPlan{Action: ActionRepayDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountAll}, func(ix *Instruction) { ix.Accounts[7].PubKey = squads.ProgramID }},
		{"repay-wrong-count", ActionPlan{Action: ActionRepayDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountAll}, func(ix *Instruction) { ix.Accounts = ix.Accounts[:12] }},
		{"swap-wrong-slippage", ActionPlan{Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(650_000)}, func(ix *Instruction) { ix.Data[len(ix.Data)-3] = 100 }},
		{"swap-wrong-recipe", ActionPlan{Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(650_000)}, func(ix *Instruction) { ix.Data[13] = 255 }},
		{"swap-wrong-pool", ActionPlan{Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(650_000)}, func(ix *Instruction) { ix.Accounts[5].PubKey = f.topology.ClaimCustody }},
		{"swap-wrong-authority", ActionPlan{Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(650_000)}, func(ix *Instruction) { ix.Accounts[1].PubKey = f.topology.Strategies[SyrupUsdcUsdc].MarketAuthority }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.plan.Action != ActionBorrowDebt && !borrowed {
				result, err := f.worker(t, false).Tick(f.ctx)
				if err != nil || result.Condition != "operation_reconciled" {
					t.Fatalf("actual borrowing: %v %v", result, err)
				}
				before, err = ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
				if err != nil {
					t.Fatal(err)
				}
				if before.Position(SyrupUsdcUsdc).DebtRaw == 0 || before.Claim.AmountRaw != before.Position(SyrupUsdcUsdc).DebtRaw {
					t.Fatal("actual SPL borrowing absent")
				}
				borrowed = true
			}
			if test.plan.Action == ActionSwapClaimToCollateral {
				test.plan.Amount = AmountExact(before.Claim.AmountRaw)
			}
			built, err := BuildOperation(&test.plan, before, f.topology, multiplyBankQuoteClient{f}, f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := f.executor.EnsureExactPolicy(f.ctx, f.topology, &test.plan, built)
			if err != nil {
				t.Fatal(err)
			}
			baseline, slot, err := f.executor.PrepareAndSign(f.ctx, built, policy.Account, 0, policy.ConstraintIndexes, before.Slot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.executor.Simulate(f.ctx, baseline, slot); err != nil {
				t.Fatalf("unmodified source recipe simulation: %v", err)
			}
			test.change(&built.PolicyInstructions[0])
			signed, slot, err := f.executor.PrepareAndSign(f.ctx, built, policy.Account, 0, policy.ConstraintIndexes, before.Slot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.executor.Simulate(f.ctx, signed, slot); err == nil {
				t.Fatal("invalid source recipe simulated successfully")
			}
		})
	}
	after, err := ObserveConfirmed(f.ctx, f.cluster, f.topology, nil)
	if err != nil || after.Position(SyrupUsdcUsdc).DebtRaw != before.Position(SyrupUsdcUsdc).DebtRaw || after.Claim.AmountRaw != before.Claim.AmountRaw || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != before.CollateralCustody(SyrupUsdcUsdc).AmountRaw {
		t.Fatal("invalid recipe simulation mutated actual financial bank")
	}
}

func TestCurrentGoMultiplyActualReceiptWaitsForCoherentBank(t *testing.T) {
	f := newMultiplySVMFixture(t)
	saved := f.landLostResponse(t)
	f.mu.Lock()
	f.staleObservation = true
	f.mu.Unlock()
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "awaiting_coherent_confirmed_observation" {
		t.Fatalf("stale provider account bank: %v %v", result, err)
	}
	held, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || held.Operation == nil || held.Operation.Status != StatusConfirmed || !bytes.Equal(held.Operation.SignedWire, saved.Operation.SignedWire) {
		t.Fatal("stale account frontier terminated/replaced financial attempt")
	}
	var receipts int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='reconciled'`, held.Operation.OperationID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("stale account frontier published financial completion")
	}
	f.mu.Lock()
	f.staleObservation = false
	f.mu.Unlock()
	result, err = f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "confirmed_operation_reconciled" {
		t.Fatalf("actual coherent frontier recovery %v %v", result, err)
	}
	f.assertReconciled(t, 1)
}
