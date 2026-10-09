package backyard

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPartialRiskAuthorizationDoesNotGrantDebtClear(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyRepayFixture(t)
	installAutoEmergencyRepaymentSimulation(t, rpc, accounts, e, o.Snapshot.Slot, nil)
	plan, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	id := sha256Bytes([]byte("partial-risk-authority"))
	proof, err := verifyDebtClearEmergency(m, o, d, id, time.Now().UTC())
	if err != nil || proof == nil {
		t.Fatal("missing actual risk", err)
	}
	auth := phase3OperationAuthorization{}
	handled, err := bindPartialRiskAuthorization(m, e.Request, e.ExpectedEffects, &plan, &auth, debtClearRouteState{}, proof, id, o.Snapshot.Slot, true)
	if err != nil || !handled || auth.PartialRisk == nil || auth.DebtClear != nil {
		t.Fatal("partial risk promoted to full clear", err)
	}
	raw, _ := json.Marshal(auth)
	var restored phase3OperationAuthorization
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if handled, err := bindPartialRiskAuthorization(m, e.Request, e.ExpectedEffects, &plan, &restored, debtClearRouteState{}, nil, id, o.Snapshot.Slot, false); err != nil || !handled || restored.DebtClear != nil {
		t.Fatal("restart lost narrow authority", err)
	}
	for _, kind := range []string{"missing", "foreign", "expired", "full payoff", "existing full authority"} {
		t.Run(kind, func(t *testing.T) {
			a := phase3OperationAuthorization{}
			p := *proof
			risk := &p
			r := e.Request
			state := debtClearRouteState{}
			switch kind {
			case "missing":
				risk = nil
			case "foreign":
				p.OperationID = sha256Bytes([]byte("other"))
			case "expired":
				p.ObservedAt = time.Now().Add(-time.Minute)
			case "full payoff":
				r.FullPayoff = true
			case "existing full authority":
				state.Authority = &debtClearAuthority{}
			}
			handled, err := bindPartialRiskAuthorization(m, r, e.ExpectedEffects, &plan, &a, state, risk, id, o.Snapshot.Slot, true)
			if !handled || err == nil || a.PartialRisk != nil || a.DebtClear != nil {
				t.Fatal("invalid proof mutated authority", err)
			}
		})
	}
	changed := e.Request
	changed.FullPayoff = true
	before, _ := json.Marshal(restored)
	if _, err := bindPartialRiskAuthorization(m, changed, e.ExpectedEffects, &plan, &restored, debtClearRouteState{}, nil, id, o.Snapshot.Slot, false); err == nil {
		t.Fatal("persisted partial proof authorized full payoff")
	}
	after, _ := json.Marshal(restored)
	if string(before) != string(after) {
		t.Fatal("rejected escalation mutated authority")
	}
}
