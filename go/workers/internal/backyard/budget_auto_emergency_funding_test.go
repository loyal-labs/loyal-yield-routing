package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func autoEmergencyFundingFixture(t *testing.T, amount ...uint64) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := autoEmergencyRepayFixture(t)
	collateral := uint64(24_000_000_000)
	if len(amount) > 0 {
		collateral = amount[0]
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], collateral)
	loan, err := observeTopupLoanOrigin(context.Background(), rpc)
	if err != nil {
		t.Fatal(err)
	}
	origin := sha256Bytes([]byte("owned-topup-allocation"))
	tranche := &topupTranche{Generation: 2, Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, LastOperationID: sha256Bytes([]byte("finalized-topup-swap")), LastSlot: o.Snapshot.Slot, LastEffectsSHA256: sha256Bytes([]byte("actual-swap-receipt")), AllocatedUSDCRaw: 2_000_000, CollateralRemainingRaw: collateral, Stage: topupTrancheCollateral}
	observed, _, err := autoObservationForAccounts(m, o.Snapshot.Slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	enrich := func(_ context.Context, fresh *Observation) error {
		fresh.Snapshot.TopupTranche = tranche
		fresh.Snapshot.PilotActive = true
		fresh.Snapshot.JournalSequenceKnown, fresh.Snapshot.JournalReconciledSequenceRaw = true, 40
		fresh.Snapshot.JournalArmedNAVKnown, fresh.Snapshot.JournalArmedNAVRaw = true, fresh.Snapshot.PriorReportedNAVRaw
		fresh.Snapshot.CapitalMutated, fresh.Snapshot.ProgramIdentityKnown = true, true
		fresh.Snapshot.VoltrProgramDeploySlot, fresh.Snapshot.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
		return nil
	}
	enrich(context.Background(), &observed)
	d := m.DecideOnManifest(observed.Snapshot)
	if d.Action != SwapCollateralToDebtStep || d.Reason != "hard_ltv_buffer_swap" {
		t.Fatalf("decision %+v", d)
	}
	for _, a := range productionRouteBatchAccounts(t, o.Snapshot.Slot, nil) {
		if accountAt(accounts, a.Address).Address == "" {
			upsertConfirmedAccount(&accounts, a)
		}
	}
	for _, address := range routeFixedAddresses(m) {
		if accountAt(accounts, address).Address == "" {
			upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: address, Owner: bridgeSquadsProgram, Lamports: 1, Data: bytes.Repeat([]byte{7}, 96)})
		}
	}
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"getProgramAccounts"`)) {
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":[]}}`, o.Snapshot.Slot)), nil
		}
		if bytes.Contains(body, []byte(`"method":"getMultipleAccounts"`)) {
			var call struct{ Params []json.RawMessage }
			json.Unmarshal(body, &call)
			var addresses []string
			json.Unmarshal(call.Params[0], &addresses)
			for _, address := range addresses {
				if accountAt(accounts, address).Address == "" {
					return inner.RoundTrip(r)
				}
			}
			var values []any
			for _, address := range addresses {
				a := accountAt(accounts, address)
				if a.Address == "" {
					values = append(values, nil)
					continue
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": o.Snapshot.Slot}, "value": values}})
			return response(string(payload)), nil
		}
		return inner.RoundTrip(r)
	})
	prepared, e, err := observeConfirmedJupiterExecutionEvidenceWithEnrichment(context.Background(), rpc, m, d, client, enrich)
	if err != nil {
		t.Fatal(err)
	}
	if e.Request.MinimumOutputRaw < uint64(prepared.Snapshot.PositionDebtRaw) {
		if !e.Request.EmergencyTopupFunding || e.Request.FullPayoffFunding {
			t.Fatal("missing explicit partial funding classification")
		}
	} else if e.Request.EmergencyTopupFunding || !e.Request.FullPayoffFunding {
		t.Fatal("large output granted partial authority")
	}
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Path = strings.TrimPrefix(request.URL.Path, "/swap/v1")
		return client.http.Transport.RoundTrip(request)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	return prepared, d, e, m, rpc, client, accounts
}

func TestAutoEmergencyFundingFullPayoffGuardPreserved(t *testing.T) {
	o, d, e, m, rpc, _, _ := autoEmergencyFundingFixture(t)
	e.Request.EmergencyTopupFunding = false
	e.Request.FullPayoffFunding = true
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	assertBudgetHold(t, runtime.admitJupiter(context.Background(), "small-owned-risk", o, d, e), "funding_quote_cannot_cover_full_payoff")
}

func installEmergencyFundingSimulation(t *testing.T, rpc *RPCClient, m RouteManifest, accounts []ConfirmedAccount, e JupiterExecutionEvidence, slot int64, mutate func([]ConfirmedAccount)) {
	t.Helper()
	message, err := m.compileJupiterMessage(e.Request, mustKey(bridgeDelegate))
	if err != nil {
		t.Fatal(err)
	}
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if err = json.Unmarshal(body, &call); err != nil {
			return nil, err
		}
		if call.Method != "simulateTransaction" {
			return inner.RoundTrip(req)
		}
		var encoded string
		var options struct{ Accounts struct{ Addresses []string } }
		if json.Unmarshal(call.Params[0], &encoded) != nil || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("bad simulation")
		}
		wire, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(wire) < 65 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) {
			t.Fatal("changed swap simulation wire")
		}
		post := make([]ConfirmedAccount, len(accounts))
		for i, a := range accounts {
			post[i] = a
			post[i].Data = append([]byte(nil), a.Data...)
		}
		for _, effect := range e.ExpectedEffects.Accounts {
			binary.LittleEndian.PutUint64(accountAt(post, effect.Address).Data[64:72], effect.AfterRaw)
		}
		if mutate != nil {
			mutate(post)
		}
		var rows []any
		for _, address := range options.Accounts.Addresses {
			a := accountAt(post, address)
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": slot}, "value": map[string]any{"err": nil, "unitsConsumed": 200000, "accounts": rows}}})
		return response(string(payload)), nil
	})
}

func TestAutoEmergencyFundingProductionDispatch(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyFundingFixture(t)
	installEmergencyFundingSimulation(t, rpc, m, accounts, e, o.Snapshot.Slot, nil)
	before, _ := json.Marshal(accounts)
	_, releaseErr := m.decodeKaminoRepaymentReleaseForMode(accounts, autoAUTOPYUSD, o.Snapshot.Slot, 7, true)
	assertBudgetHold(t, releaseErr, "no_safe_repayment_collateral_release")
	plan, err := observePhase3EmergencyTopupFundingAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(accounts)
	if !bytes.Equal(before, after) || plan.Snapshot != o.Snapshot || plan.EmergencyTopupFunding == nil || plan.ExitCycles < 1 {
		t.Fatal("current state changed or incomplete plan")
	}
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	assertBudgetHold(t, runtime.admitJupiter(context.Background(), "small-owned-risk", o, d, e), "bridge_admission_database_unavailable")
	proofJSON, _ := json.Marshal(plan)
	for _, variant := range []string{"virtual-before", "virtual-after", "missing-before", "foreign-before", "loan-byte", "clock-byte", "repay-amount", "forged-before-slot", "expired-before-slot"} {
		t.Run("proof-"+variant, func(t *testing.T) {
			var changed phase3BridgeAdmission
			json.Unmarshal(proofJSON, &changed)
			proof := changed.EmergencyTopupFunding
			switch variant {
			case "virtual-before":
				proof.Before[0].ValuationSource = routeRefreshValuationSource
			case "virtual-after":
				proof.Projection.Accounts[0].ValuationSource = routeRefreshValuationSource
			case "missing-before":
				proof.Before = proof.Before[1:]
			case "foreign-before":
				proof.Before[0].Owner = bridgeSquadsProgram
			case "loan-byte":
				accountAt(proof.Projection.Accounts, autoAUTOPYUSD.Kamino.Obligation).Data[1296] ^= 1
			case "clock-byte":
				accountAt(proof.Projection.Accounts, budgetClockAddress).Data[0] ^= 1
			case "repay-amount":
				proof.RepaymentRaw++
			case "forged-before-slot":
				proof.BeforeSlot = proof.Projection.Slot + 1
			case "expired-before-slot":
				proof.BeforeSlot = changed.Snapshot.Slot - 1
			}
			before, _ := json.Marshal(changed)
			if err := validateEmergencyTopupFundingProof(m, &changed); err == nil {
				t.Fatal("forged historical proof accepted")
			}
			after, _ := json.Marshal(changed)
			if !bytes.Equal(before, after) {
				t.Fatal("proof validation mutated pricing")
			}
		})
	}
	current, effects, wire, err := plan.Input.decodeWithManifest(m)
	wantWire, wireErr := m.compileJupiterMessage(e.Request, mustKey(bridgeDelegate))
	wantJSON, _ := json.Marshal(e.Request)
	gotJSON, _ := json.Marshal(current)
	if err != nil || wireErr != nil || !bytes.Equal(wire, wantWire) || !bytes.Equal(wantJSON, gotJSON) || plan.ValidThroughSlot > o.Snapshot.Slot+observationLagSlots() {
		t.Fatal("current input or deadline changed")
	}
	id := sha256Bytes([]byte("funding-authority"))
	risk, err := verifyDebtClearEmergency(m, o, d, id, time.Now().UTC())
	if err != nil || risk == nil {
		t.Fatal(err)
	}
	intent, err := Phase3IntentDigest(e.Request, plan.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	handoff := topupHandoffAuthority{OriginOperationID: id, AuthoritySHA256: intent}
	tranche := o.Snapshot.TopupTranche
	binding := &topupTrancheBinding{Loan: tranche.Loan, Lane: tranche.Lane, OriginOperationID: tranche.OriginOperationID, AllocatedUSDCRaw: tranche.AllocatedUSDCRaw, Before: tranche, Handoff: handoff}
	signed := append(make([]byte, 65), wire...)
	signed[0] = 1
	auth := phase3OperationAuthorization{PilotAuthorityID: pilotBudgetAuthorityID, GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: plan.Input, BridgeAdmission: &plan, SignedWireSHA256: sha256Bytes(signed), Topup: binding, PartialRisk: risk}
	encoded, _ := json.Marshal(auth)
	if err = json.Unmarshal(encoded, &auth); err != nil {
		t.Fatal(err)
	}
	if err = validateEmergencyTopupFundingAuthorization(context.Background(), rpc, m, auth, o.Snapshot.Slot); err != nil {
		t.Fatal("fresh build helper", err)
	}
	op := PersistedOperation{Status: Signed, SignedWire: signed, SignedWireSHA256: sha256Bytes(signed), TransactionSignature: encodeBase58(signed[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	if _, err = m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal("fresh signed send", err)
	}
	_ = effects
	for _, variant := range []string{"missing-risk", "missing-binding", "expired", "marker", "principal", "policy", "program", "custody", "wire"} {
		t.Run(variant, func(t *testing.T) {
			var candidate phase3OperationAuthorization
			json.Unmarshal(encoded, &candidate)
			operation := op
			var restore func()
			switch variant {
			case "missing-risk":
				candidate.PartialRisk = nil
			case "missing-binding":
				candidate.Topup = nil
			case "expired":
				candidate.BridgeAdmission.ValidThroughSlot = o.Snapshot.Slot - 1
			case "marker":
				a := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data
				a[1288] ^= 1
				restore = func() { a[1288] ^= 1 }
			case "principal":
				a := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data
				a[1296] ^= 1
				restore = func() { a[1296] ^= 1 }
			case "policy":
				a := accountAt(accounts, e.Request.Policy).Data
				a[0] ^= 1
				restore = func() { a[0] ^= 1 }
			case "program":
				a := accountAt(accounts, reviewedTopupKaminoIdentity().programData).Data
				a[100] ^= 1
				restore = func() { a[100] ^= 1 }
			case "custody":
				a := accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data
				a[64] ^= 1
				restore = func() { a[64] ^= 1 }
			case "wire":
				operation.SignedWire = append([]byte(nil), signed...)
				operation.SignedWire[len(operation.SignedWire)-1] ^= 1
			}
			if restore != nil {
				defer restore()
			}
			if variant != "wire" {
				if err := validateEmergencyTopupFundingAuthorization(context.Background(), rpc, m, candidate, o.Snapshot.Slot); err == nil {
					t.Fatal("fresh build accepted drift")
				}
			}
			if _, err := m.revaluePhase3SignedInput(context.Background(), rpc, candidate, operation); err == nil {
				t.Fatal("signed send accepted drift")
			}
		})
	}

}

func TestAutoEmergencyFundingSmallBufferCapAndLargeOutput(t *testing.T) {
	t.Run("finite-cycle-cap", func(t *testing.T) {
		o, d, e, m, rpc, _, accounts := autoEmergencyFundingFixture(t, 12_000_000_000)
		installEmergencyFundingSimulation(t, rpc, m, accounts, e, o.Snapshot.Slot, nil)
		runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
		assertBudgetHold(t, runtime.admitJupiter(context.Background(), "small-owned-risk", o, d, e), "leverage_exit_cycles_exceeded")
	})
	t.Run("whole-debt-minimum", func(t *testing.T) {
		_, _, e, _, _, _, _ := autoEmergencyFundingFixture(t, 48_000_000_000)
		if e.Request.EmergencyTopupFunding || !e.Request.FullPayoffFunding {
			t.Fatal("full payoff bypassed")
		}
	})
}

func TestAutoEmergencyFundingAdmissionRejectsInvalidScope(t *testing.T) {
	o, d, e, m, rpc, _, accounts := autoEmergencyFundingFixture(t)
	installEmergencyFundingSimulation(t, rpc, m, accounts, e, o.Snapshot.Slot, nil)
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	for _, variant := range []string{"missing-origin", "foreign-custody", "ordinary", "reason", "lane", "full-flag", "zero-minimum", "minimum-wire", "no-risk", "stale-risk"} {
		t.Run(variant, func(t *testing.T) {
			observation, decision, evidence := o, d, e
			tranche := *o.Snapshot.TopupTranche
			observation.Snapshot.TopupTranche = &tranche
			reason := "emergency_topup_funding_admission_unavailable"
			switch variant {
			case "missing-origin":
				observation.Snapshot.TopupTranche = nil
			case "foreign-custody":
				observation.Snapshot.CollateralIdleRaw++
				observation.Snapshot.PrimeIdleRaw++
			case "ordinary":
				tranche.Stage = topupTrancheOrdinary
			case "reason":
				decision.Reason = exitCycleSwapReason
			case "lane":
				decision.StrategyKey = onreONycUSDC
			case "full-flag":
				evidence.Request.FullPayoffFunding = true
			case "zero-minimum":
				evidence.Request.MinimumOutputRaw = 0
			case "minimum-wire":
				evidence.Request.MinimumOutputRaw++
				reason = "emergency_topup_funding_minimum_invalid"
			case "no-risk":
				observation.routeBatch = nil
				reason = "debt_clear_emergency_evidence_unavailable"
			case "stale-risk":
				observation.ObservedAt = time.Now().Add(-time.Minute)
				reason = "debt_clear_emergency_evidence_unavailable"
			}
			before, _ := json.Marshal(accounts)
			err := runtime.admitJupiter(context.Background(), "invalid-owned-risk", observation, decision, evidence)
			assertBudgetHold(t, err, reason)
			after, _ := json.Marshal(accounts)
			if !bytes.Equal(before, after) {
				t.Fatal("rejection mutated accounts")
			}
		})
	}
}

func TestAutoEmergencyFundingRejectsSwapPrincipalMutation(t *testing.T) {
	o, d, e, m, rpc, _, accounts := autoEmergencyFundingFixture(t)
	installEmergencyFundingSimulation(t, rpc, m, accounts, e, o.Snapshot.Slot, func(post []ConfirmedAccount) { accountAt(post, autoAUTOPYUSD.Kamino.Obligation).Data[1296] ^= 1 })
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	assertBudgetHold(t, runtime.admitJupiter(context.Background(), "principal-mutated-risk", o, d, e), "emergency_topup_funding_principal_changed")
}

func TestAutoEmergencyFundingResidualAndRecoveredRisk(t *testing.T) {
	for _, variant := range []string{"residual", "recovered"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, _, accounts := autoEmergencyFundingFixture(t)
			installEmergencyFundingSimulation(t, rpc, m, accounts, e, o.Snapshot.Slot, nil)
			reason := "emergency_topup_funding_residual_too_small"
			if variant == "residual" {
				data := accountAt(accounts, autoAUTOPYUSD.Kamino.Market).Data
				putScaledFraction(data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16], new(big.Int).Lsh(big.NewInt(6), 60))
			} else {
				data := accountAt(accounts, autoAUTOPYUSD.Kamino.CollateralReserve).Data[248:264]
				putScaledFraction(data, new(big.Int).Mul(littleInt(data), big.NewInt(2)))
				reason = "auto_partial_repayment_fresh_risk_required"
			}
			runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
			assertBudgetHold(t, runtime.admitJupiter(context.Background(), "invalid-risk", o, d, e), reason)
		})
	}
}
