package backyard

import (
	"encoding/json"
	"testing"
)

func TestTopupHandoffConservesDirectStageThroughRestartAndPartialConsumption(t *testing.T) {
	loan, _ := topupLoanFixture(t)
	origin := sha256Bytes([]byte("allocation"))
	before := topupTranche{Generation: 1, Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, AllocatedUSDCRaw: 100,
		USDCRemainingRaw: 100, Stage: topupTrancheAllocated, LastOperationID: origin, LastSlot: 52, LastEffectsSHA256: sha256Bytes([]byte("allocation receipt"))}
	authority := topupHandoffAuthority{OriginOperationID: sha256Bytes([]byte("authorized withdrawal origin")), AuthoritySHA256: sha256Bytes([]byte("immutable withdrawal authority"))}
	apply := func(before topupTranche, action Action, amount int64, slot int64) *topupTranche {
		t.Helper()
		binding := topupTrancheBinding{Loan: before.Loan, Lane: before.Lane, OriginOperationID: before.OriginOperationID, AllocatedUSDCRaw: before.AllocatedUSDCRaw, Before: &before}
		e, strategy, squads, err := bridgeExpectedEffects(Decision{Action: action, AmountRaw: amount}, 400, before.StrategyRemainingRaw, before.USDCRemainingRaw)
		if err != nil {
			t.Fatal(err)
		}
		e.Kind = "bridge"
		idle := uint64(400)
		if action == VoltrRestoreIdle {
			idle += uint64(amount)
		}
		after := []uint64{strategy, squads}
		if action != StageSquadsToVoltr {
			e.ReturnData = expectedAdaptorReturnData(strategy + squads)
			after = append([]uint64{idle}, after...)
		}
		r := topupReceipt(t, e, slot, after...)
		id := sha256Bytes([]byte(r.Signature + string(action)))
		next, err := reconcileTopupHandoff(before, binding, authority, id, action, e, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileTopupHandoff(*next, binding, authority, id, action, e, r); err == nil {
			t.Fatal("handoff replay adopted stale inventory")
		}
		raw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		restarted, err := decodeTopupTranche(raw)
		if err != nil {
			t.Fatal(err)
		}
		return restarted
	}
	staged := apply(before, StageSquadsToVoltr, 60, 53)
	if staged.Stage != topupTrancheHandoff || staged.USDCRemainingRaw != 40 || staged.StrategyRemainingRaw != 60 {
		t.Fatal("partial transfer dropped ownership", staged)
	}
	// There is intentionally no demand input: shrinking demand cannot erase
	// the already-authorized origin or any remaining inventory at reconciliation.
	restored := apply(*staged, VoltrRestoreIdle, 60, 54)
	if restored.Stage != topupTrancheHandoff || restored.USDCRemainingRaw != 40 || restored.StrategyRemainingRaw != 0 {
		t.Fatal("demand disappearance cleared remaining inventory", restored)
	}
	staged = apply(*restored, StageSquadsToVoltr, 40, 55)
	complete := apply(*staged, VoltrRestoreIdle, 40, 56)
	if complete.Stage != topupTrancheComplete || complete.Handoff != authority || complete.USDCRemainingRaw != 0 || complete.StrategyRemainingRaw != 0 {
		t.Fatal("terminal handoff did not conserve ownership", complete)
	}
	binding := topupTrancheBinding{Loan: before.Loan, Lane: before.Lane, OriginOperationID: before.OriginOperationID, AllocatedUSDCRaw: before.AllocatedUSDCRaw, Before: &before}
	e, _, _, err := bridgeExpectedEffects(Decision{Action: StageSquadsToVoltr, AmountRaw: 100}, 400, 0, 101)
	if err != nil {
		t.Fatal(err)
	}
	e.Kind = "bridge"
	if _, err := reconcileTopupHandoff(before, binding, authority, authority.OriginOperationID, StageSquadsToVoltr, e, topupReceipt(t, e, 53, 100, 1)); err == nil {
		t.Fatal("foreign cash adopted at handoff")
	}
}
