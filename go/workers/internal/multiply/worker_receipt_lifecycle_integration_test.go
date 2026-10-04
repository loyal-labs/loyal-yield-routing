package multiply

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

// Publish an actual SDK-constrained, simulated wire using the same durable
// publication APIs, then stop before intent. No transaction or poststate is
// fabricated: the following keyless Tick must execute/reconcile it itself.
func (f *multiplySVMFixture) persistUnsentDeposit(t *testing.T) *MultiplyOperation {
	t.Helper()
	before, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
	if outcome, err := f.executor.Simulate(f.ctx, signed, slot); err != nil || outcome.Err != nil {
		t.Fatalf("actual prepublication simulation: %v %v", outcome, err)
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
	prestate, err := NewOperationPrestate(op, before, f.topology)
	if err != nil {
		t.Fatal(err)
	}
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.PersistSignedOperationWithPrestate(f.ctx, lease, id, policy.Account.String(), policy.DataSHA256, messageHash, signed, prestate); err != nil || !ok {
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
	if err != nil || result.Condition != "recovered_operation_reconciled" {
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
	if result, err := f.executor.Simulate(f.ctx, &SignedOperation{Wire: original.SignedWire}, *original.LastValidBlockHeight+1); err != nil || result.Err == nil {
		t.Fatalf("obsolete actual bank wire still valid: %v %v", result, err)
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
	var evidence []byte
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT o.status,e.evidence FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_operation_evidence e USING(operation_id) WHERE o.operation_id=$1 AND e.evidence_kind='expired_no_effect'`, original.OperationID).Scan(&status, &evidence); err != nil || status != "expired" || !bytes.Contains(evidence, []byte(*original.SignedWireSHA256)) {
		t.Fatalf("immutable actual no-effect evidence: %s %v", status, err)
	}
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
	if err != nil || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 1_000_000 {
		t.Fatal("actual withdrawal financial state differs from receipt")
	}
	saved, err = f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation != nil || saved.State.Withdrawal.Status != WithdrawalRequested || saved.State.Withdrawal.AmountRaw != 1_000_000 || saved.State.Withdrawal.DestinationAccount != fixtureKey(120).String() {
		t.Fatal("non-USDC unwind fabricated payout completion or changed request")
	}
	var receipts int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operation_evidence e JOIN loyal_yield.multiply_operations o USING(operation_id) WHERE o.route_key=$1 AND e.evidence_kind='reconciled_receipt'`, f.state.RouteKey).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatalf("deposit and unwind receipts: %d %v", receipts, err)
	}
}

func TestCurrentGoMultiplyActualReceiptTerminalRollsBackOnStaleFence(t *testing.T) {
	f := newMultiplySVMFixture(t)
	f.mu.Lock()
	f.lostResponse = true
	f.mu.Unlock()
	if _, err := f.worker(t, false).Tick(f.ctx); err == nil {
		t.Fatal("response loss was hidden")
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation == nil {
		t.Fatal("actual sent journal missing")
	}
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
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operation_evidence WHERE operation_id=$1 AND evidence_kind='reconciled_receipt'`, op.OperationID).Scan(&n); err != nil || n != 0 {
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
			before, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
			outcome, err := f.executor.Simulate(f.ctx, signed, slot)
			if err == nil && outcome.Err == nil {
				t.Fatal("invalid oracle fixture vector simulated successfully")
			}
		})
	}
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
	if err != nil || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 1_000_000 || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 0 {
		t.Fatal("invalid simulation changed financial bank")
	}
}

func TestCurrentGoMultiplyActualReceiptRejectsProviderDrift(t *testing.T) {
	f := newMultiplySVMFixture(t)
	f.mu.Lock()
	f.lostResponse = true
	f.mu.Unlock()
	if _, err := f.worker(t, false).Tick(f.ctx); err == nil {
		t.Fatal("response loss was hidden")
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation == nil {
		t.Fatal("actual financial journal missing")
	}
	raw, err := f.rpc.ConfirmedTransaction(f.ctx, *saved.Operation.TransactionSignature)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateConfirmedReceipt(saved.Operation, f.topology, raw, nil); err != nil {
		t.Fatalf("unmodified actual bank receipt: %v", err)
	}
	for _, mutation := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing-success", func(r map[string]any) { delete(r["meta"].(map[string]any), "err") }},
		{"failed-transaction", func(r map[string]any) { r["meta"].(map[string]any)["err"] = "failed" }},
		{"unknown-slot", func(r map[string]any) { r["slot"] = 0 }},
		{"different-wire", func(r map[string]any) { r["transaction"].([]any)[0] = "AA==" }},
		{"incomplete-sol", func(r map[string]any) { r["meta"].(map[string]any)["postBalances"] = []any{} }},
		{"different-loaded-order", func(r map[string]any) {
			r["meta"].(map[string]any)["loadedAddresses"] = map[string]any{"writable": []any{fixtureKey(122).String()}, "readonly": []any{}}
		}},
		{"missing-token-index", func(r map[string]any) {
			tokens := r["meta"].(map[string]any)["postTokenBalances"].([]any)
			delete(tokens[0].(map[string]any), "accountIndex")
		}},
		{"duplicate-token-index", func(r map[string]any) {
			meta := r["meta"].(map[string]any)
			tokens := meta["postTokenBalances"].([]any)
			meta["postTokenBalances"] = append(tokens, tokens[0])
		}},
		{"foreign-token-authority", func(r map[string]any) {
			tokens := r["meta"].(map[string]any)["postTokenBalances"].([]any)
			for _, tok := range tokens {
				tok.(map[string]any)["owner"] = fixtureKey(123).String()
			}
		}},
		{"foreign-token-program", func(r map[string]any) {
			tokens := r["meta"].(map[string]any)["postTokenBalances"].([]any)
			for _, tok := range tokens {
				tok.(map[string]any)["programId"] = SquadsProgram
			}
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			var r map[string]any
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			mutation.change(r)
			changed, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validateConfirmedReceipt(saved.Operation, f.topology, changed, nil); err == nil {
				t.Fatal("provider drift accepted as actual financial receipt")
			}
		})
	}
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "recovered_operation_reconciled" {
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
			f := newMultiplySVMFixtureWithInitialAccounts(t, func(topology *EarnMaxTopology, accounts map[string]*Account) {
				for _, key := range test.keys {
					config := topology.Strategies[key]
					data := make([]byte, obligationLength)
					copy(data[:8], obligationDiscriminator)
					binary.LittleEndian.PutUint64(data[16:24], 1000)
					copy(data[32:64], config.Market[:])
					copy(data[64:96], topology.Vault[:])
					copy(data[96:128], config.CollateralReserve[:])
					binary.LittleEndian.PutUint64(data[128:136], 1000)
					accounts[config.Obligation.String()] = &Account{Address: config.Obligation.String(), Owner: KlendProgram, Data: data, Lamports: 10_000_000}
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
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
				if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operation_evidence WHERE operation_id=$1`, held.Operation.OperationID).Scan(&receipts); err != nil || receipts != 0 {
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
	after, err = ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(DISTINCT action),count(*) FILTER(WHERE e.operation_id IS NULL) FROM loyal_yield.multiply_operations o LEFT JOIN loyal_yield.multiply_operation_evidence e ON e.operation_id=o.operation_id AND e.evidence_kind='reconciled_receipt' WHERE o.route_key=$1 AND o.status='reconciled'`, f.state.RouteKey).Scan(&kinds, &missing); err != nil || kinds < 6 || missing != 0 {
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
	before, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
		{"repay-wrong-token-program", ActionPlan{Action: ActionRepayDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountAll}, func(ix *Instruction) { ix.Accounts[7].PubKey = mustKey(SquadsProgram) }},
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
				before, err = ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
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
			if outcome, err := f.executor.Simulate(f.ctx, baseline, slot); err != nil || outcome.Err != nil {
				t.Fatalf("unmodified source recipe simulation: %v %v", outcome, err)
			}
			test.change(&built.PolicyInstructions[0])
			signed, slot, err := f.executor.PrepareAndSign(f.ctx, built, policy.Account, 0, policy.ConstraintIndexes, before.Slot)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.executor.Simulate(f.ctx, signed, slot)
			if err == nil && result.Err == nil {
				t.Fatal("invalid source recipe simulated successfully")
			}
		})
	}
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
	if err != nil || after.Position(SyrupUsdcUsdc).DebtRaw != before.Position(SyrupUsdcUsdc).DebtRaw || after.Claim.AmountRaw != before.Claim.AmountRaw || after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != before.CollateralCustody(SyrupUsdcUsdc).AmountRaw {
		t.Fatal("invalid recipe simulation mutated actual financial bank")
	}
}

func TestCurrentGoMultiplyActualReceiptWaitsForCoherentBank(t *testing.T) {
	f := newMultiplySVMFixture(t)
	f.mu.Lock()
	f.lostResponse = true
	f.mu.Unlock()
	if _, err := f.worker(t, false).Tick(f.ctx); err == nil {
		t.Fatal("actual response loss was hidden")
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation == nil {
		t.Fatal("actual financial journal absent")
	}
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
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operation_evidence WHERE operation_id=$1 AND evidence_kind='reconciled_receipt'`, held.Operation.OperationID).Scan(&receipts); err != nil || receipts != 0 {
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
