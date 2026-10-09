package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func autoEmergencyRepayFixture(t *testing.T, cash ...uint64) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	_, _, _, m, _, _, accounts := debtTopupAllocationFixture(t)
	route := autoAUTOPYUSD
	client := autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
		out := amount
		if in == route.Kamino.CollateralMint {
			out = amount / 6000
		}
		if destination == route.Kamino.CollateralMint {
			out = amount * 6000
		}
		return out, out * 9950 / 10000
	}, nil)
	slot := reviewedTopupKaminoIdentity().deploySlot + 1
	for _, a := range accounts {
		if a.Owner == kaminoProgram && len(a.Data) > 32 {
			binary.LittleEndian.PutUint64(a.Data[16:24], uint64(slot))
		}
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], 0)
	amount := uint64(4_000_000)
	if len(cash) > 0 {
		amount = cash[0]
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], amount)
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := reserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Price the actual 7 PYUSD loan against 10 USDC of collateral (70% LTV).
	price := new(big.Int).Lsh(big.NewInt(10_000_000_000), 60)
	price.Quo(price, new(big.Int).SetUint64(redeemed))
	putScaledFraction(accountAt(accounts, route.Kamino.CollateralReserve).Data[248:264], price)
	for _, lane := range selectorObservationLanes(m) {
		other, _ := runtimeRoute(lane)
		if lane != route.Lane {
			upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: other.Kamino.Obligation, Owner: "11111111111111111111111111111111"})
			a := tokenAccountFixture(t, other.CollateralCustody, other.Kamino.CollateralMint, bridgeVault, 0)
			a.Owner = other.CollateralTokenProgram
			upsertConfirmedAccount(&accounts, a)
		}
	}
	m.selectorObservation = true
	o, _, err := autoObservationForAccounts(m, slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o.ObservedAt = time.Now().UTC()
	o.Snapshot.PilotActive = true
	o.Snapshot.JournalSequenceKnown, o.Snapshot.JournalReconciledSequenceRaw = true, 40
	o.Snapshot.JournalArmedNAVKnown, o.Snapshot.JournalArmedNAVRaw = true, o.Snapshot.PriorReportedNAVRaw
	o.Snapshot.CapitalMutated, o.Snapshot.ProgramIdentityKnown = true, true
	o.Snapshot.VoltrProgramDeploySlot, o.Snapshot.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
	o.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("emergency-partial"))
	d := m.DecideOnManifest(o.Snapshot)
	if d.Reason != "hard_ltv_repay" || d.AmountRaw != int64(amount) {
		t.Fatalf("decision %+v snapshot %+v", d, o.Snapshot)
	}
	r, err := m.kaminoPacketForRoute(d.Action, kaminoLegRepay, uint64(d.AmountRaw), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, r.AmountRaw, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	rpc := autoCleanupRPC(t, slot, accounts)
	actual := topupLoanRPC(t, accounts, "")
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(reviewedTopupKaminoIdentity().programData)) {
			return actual.client.Transport.RoundTrip(req)
		}
		return inner.RoundTrip(req)
	})
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func TestAutoEmergencyPartialRepaymentProductionDispatch(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyRepayFixture(t)
	installAutoEmergencyRepaymentSimulation(t, rpc, accounts, e, o.Snapshot.Slot, nil)
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Path = strings.TrimPrefix(request.URL.Path, "/swap/v1")
		return client.http.Transport.RoundTrip(request)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	assertBudgetHold(t, runtime.admitKamino(context.Background(), "auto-risk", o, d, e), "bridge_admission_database_unavailable")
}

func installAutoEmergencyRepaymentSimulation(t *testing.T, rpc *RPCClient, accounts []ConfirmedAccount, e KaminoExecutionEvidence, slot int64, mutate func([]ConfirmedAccount)) {
	t.Helper()
	message, err := CompileKaminoMessage(e.Request)
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
			t.Fatal("changed simulation wire")
		}
		projected := make([]ConfirmedAccount, len(accounts))
		for i, a := range accounts {
			projected[i] = a
			projected[i].Data = append([]byte(nil), a.Data...)
		}
		for _, effect := range e.ExpectedEffects.Accounts {
			binary.LittleEndian.PutUint64(accountAt(projected, effect.Address).Data[64:72], effect.AfterRaw)
		}
		obligation := accountAt(projected, autoAUTOPYUSD.Kamino.Obligation)
		amount := new(big.Int).Sub(littleInt(obligation.Data[1296:1312]), new(big.Int).Lsh(new(big.Int).SetUint64(e.Request.AmountRaw), 60))
		putScaledFraction(obligation.Data[1296:1312], amount)
		if mutate != nil {
			mutate(projected)
		}
		var rows []any
		for _, address := range options.Accounts.Addresses {
			a := accountAt(projected, address)
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": slot}, "value": map[string]any{"err": nil, "unitsConsumed": 200000, "accounts": rows}}})
		return response(string(payload)), nil
	})
}

func TestAutoEmergencyPartialRepaymentExactPrincipal(t *testing.T) {
	o, _, e, _, _, _, before := autoEmergencyRepayFixture(t)
	route := autoAUTOPYUSD
	for _, variant := range []string{"exact", "accrued", "fractional", "marker", "full", "reserve_order", "minimum"} {
		t.Run(variant, func(t *testing.T) {
			after := make([]ConfirmedAccount, len(before))
			for i, a := range before {
				after[i] = a
				after[i].Data = append([]byte(nil), a.Data...)
			}
			r := e.Request
			old, _ := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
			debt := littleInt(old.debtAmountSF[:])
			if variant == "accrued" {
				rate := new(big.Int).Add(littleInt(old.cumulativeBorrowRate[:]), big.NewInt(123))
				putScaledFraction(accountAt(after, route.Kamino.DebtReserve).Data[296:328], rate)
				putScaledFraction(accountAt(after, route.Kamino.Obligation).Data[1240:1272], rate)
				debt.Mul(debt, rate).Quo(debt, littleInt(old.cumulativeBorrowRate[:]))
			}
			debt.Sub(debt, new(big.Int).Lsh(new(big.Int).SetUint64(r.AmountRaw), 60))
			if variant == "fractional" {
				debt.Sub(debt, big.NewInt(1))
			}
			putScaledFraction(accountAt(after, route.Kamino.Obligation).Data[1296:1312], debt)
			if variant == "marker" {
				accountAt(after, route.Kamino.Obligation).Data[1288] ^= 1
			}
			if variant == "full" {
				r.AmountRaw = uint64(o.Snapshot.PositionDebtRaw)
			}
			if variant == "reserve_order" {
				r.ObligationReserves = []string{route.Kamino.DebtReserve, route.Kamino.CollateralReserve}
			}
			if variant == "minimum" {
				putScaledFraction(accountAt(after, route.Kamino.Market).Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16], new(big.Int).Lsh(big.NewInt(6), 60))
			}
			err := validateAutoPartialRepaymentPrincipal(before, after, r)
			if (variant == "exact" || variant == "accrued") != (err == nil) {
				t.Fatalf("%s: %v", variant, err)
			}
		})
	}
}

func TestAutoEmergencyPartialRepaymentFreshProof(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyRepayFixture(t)
	variant := ""
	installAutoEmergencyRepaymentSimulation(t, rpc, accounts, e, o.Snapshot.Slot, func(after []ConfirmedAccount) {
		a := accountAt(after, autoAUTOPYUSD.Kamino.Obligation)
		if variant == "fractional" {
			putScaledFraction(a.Data[1296:1312], new(big.Int).Sub(littleInt(a.Data[1296:1312]), big.NewInt(1)))
		}
		if variant == "marker" {
			a.Data[1288] ^= 1
		}
	})
	plan, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RepaymentProjection == nil || plan.Payoff == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil || len(plan.Exit) == 0 || plan.ExitAfterMicros <= 0 || plan.Snapshot != o.Snapshot {
		t.Fatal("incomplete remaining-position return")
	}
	if plan.ValidThroughSlot > o.Snapshot.Slot+observationLagSlots() {
		t.Fatal("deadline widened")
	}
	for _, v := range []string{"", "fractional", "marker"} {
		variant = v
		_, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, o.Snapshot.Slot)
		if (v == "") != (err == nil) {
			t.Fatalf("fresh %q: %v", v, err)
		}
	}
	variant = ""
	// Revalue the same persisted input after a JSON restart. Zero signature is
	// an inert offline envelope; no signer or broadcast function is involved.
	_, _, message, err := plan.Input.decodeWithManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, err := Phase3IntentDigest(e.Request, plan.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	auth := phase3OperationAuthorization{PilotAuthorityID: pilotBudgetAuthorityID, GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: plan.Input, BridgeAdmission: &plan, SignedWireSHA256: sha256Bytes(wire)}
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &auth); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	if _, err = m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal("fresh signed revaluation", err)
	}
	variant = "fractional"
	if _, err = m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
		t.Fatal("signed revaluation accepted fractional debt mutation")
	}
	variant = ""
	// Actual prestate mutation, not only a simulated poststate mutation.
	a := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation)
	a.Data[1288] ^= 1
	if _, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, o.Snapshot.Slot); err == nil {
		t.Fatal("fresh borrow marker drift admitted")
	}
	a.Data[1288] ^= 1
	price := accountAt(accounts, autoAUTOPYUSD.Kamino.CollateralReserve).Data[248:264]
	putScaledFraction(price, new(big.Int).Mul(littleInt(price), big.NewInt(2)))
	if _, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, o.Snapshot.Slot); err == nil {
		t.Fatal("risk-recovered continuation invented")
	}
}

func TestAutoEmergencyPartialRepaymentRejectsScope(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyRepayFixture(t)
	installAutoEmergencyRepaymentSimulation(t, rpc, accounts, e, o.Snapshot.Slot, nil)
	for _, v := range []string{"missing_risk", "stale", "reason", "lane", "wire", "equal", "over", "full_flag", "reserves"} {
		t.Run(v, func(t *testing.T) {
			observation, decision, evidence := o, d, e
			switch v {
			case "missing_risk":
				observation.routeBatch = nil
			case "stale":
				observation.ObservedAt = time.Now().Add(-time.Minute)
			case "reason":
				decision.Reason = "withdrawal_repay_debt"
			case "lane":
				decision.StrategyKey = onreONycUSDC
			case "wire":
				evidence.Request.AmountRaw--
			case "equal":
				decision.AmountRaw = observation.Snapshot.PositionDebtRaw
				evidence.Request.AmountRaw = uint64(decision.AmountRaw)
			case "over":
				decision.AmountRaw = observation.Snapshot.PositionDebtRaw + 1
				evidence.Request.AmountRaw = uint64(decision.AmountRaw)
			case "full_flag":
				evidence.Request.FullPayoff = true
			case "reserves":
				evidence.Request.ObligationReserves = []string{autoAUTOPYUSD.Kamino.DebtReserve, autoAUTOPYUSD.Kamino.CollateralReserve}
			}
			if _, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, observation, decision, evidence); err == nil {
				t.Fatal("invalid partial risk repayment admitted")
			}
			if v == "full_flag" {
				assertBudgetHold(t, (&Database{}).admitPhase3Withdrawal(context.Background(), rpc, client, m, "risk", observation, decision, evidence), "auto_partial_repayment_full_close_forbidden")
			}
		})
	}
}

func TestAutoEmergencyPartialRepaymentSmallBufferReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := autoEmergencyRepayFixture(t, 2_000_000)
	installAutoEmergencyRepaymentSimulation(t, rpc, accounts, e, o.Snapshot.Slot, nil)
	before, _ := json.Marshal(accounts)
	plan, err := observePhase3PartialRepaymentAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExitCycles != 2 {
		t.Fatalf("cycles=%d", plan.ExitCycles)
	}
	if _, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, o.Snapshot.Slot); err != nil {
		t.Fatal(err)
	}
	current, currentEffects, _, err := plan.Input.decodeWithManifest(m)
	currentJSON, _ := json.Marshal(current)
	expectedJSON, _ := json.Marshal(e.Request)
	effectsJSON, _ := json.Marshal(currentEffects)
	expectedEffectsJSON, _ := json.Marshal(e.ExpectedEffects)
	after, _ := json.Marshal(accounts)
	if err != nil || !bytes.Equal(currentJSON, expectedJSON) || !bytes.Equal(effectsJSON, expectedEffectsJSON) || !bytes.Equal(before, after) || plan.Snapshot != o.Snapshot || plan.ValidThroughSlot > o.Snapshot.Slot+observationLagSlots() {
		t.Fatal("current input/accounts/deadline changed", err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Path = strings.TrimPrefix(request.URL.Path, "/swap/v1")
		return client.http.Transport.RoundTrip(request)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
	assertBudgetHold(t, runtime.admitKamino(context.Background(), "small-risk", o, d, e), "bridge_admission_database_unavailable")
	for _, variant := range []string{"rate", "expired"} {
		t.Run(variant, func(t *testing.T) {
			fresh := *plan.RepaymentProjection
			fresh.Accounts = append([]ConfirmedAccount(nil), fresh.Accounts...)
			for i := range fresh.Accounts {
				fresh.Accounts[i].Data = append([]byte(nil), fresh.Accounts[i].Data...)
			}
			if variant == "rate" {
				binary.LittleEndian.PutUint32(accountAt(fresh.Accounts, autoAUTOPYUSD.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+64+10*8+4:], 100_000)
			} else {
				fresh.Slot = plan.ValidThroughSlot + 1
			}
			if err := validatePilotReleaseProjection(&plan, *plan.RepaymentProjection, fresh, autoAUTOPYUSD); err == nil {
				t.Fatal("fresh drift accepted")
			}
		})
	}
}

func TestAutoEmergencyPartialRepaymentRefreshRiskKeepsRawPrincipal(t *testing.T) {
	o, _, _, m, rpc, _, accounts := autoEmergencyRepayFixture(t)
	route := autoAUTOPYUSD
	rawObligation := append([]byte(nil), accountAt(accounts, route.Kamino.Obligation).Data...)
	rawDebtReserve := accountAt(accounts, route.Kamino.DebtReserve)
	binary.LittleEndian.PutUint64(rawDebtReserve.Data[16:24], uint64(o.Snapshot.Slot-1000))
	inner := rpc.client.Transport
	captures := 0
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
			res, err := inner.RoundTrip(req)
			if err != nil || captures == 0 || call.Method != "getMultipleAccounts" {
				return res, err
			}
			// Oracle liveness reads after the refreshed bank have advanced too;
			// the original raw capture above remains at its original context slot.
			defer res.Body.Close()
			var payload map[string]any
			if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
				return nil, err
			}
			payload["result"].(map[string]any)["context"].(map[string]any)["slot"] = o.Snapshot.Slot + 1
			encoded, _ := json.Marshal(payload)
			return response(string(encoded)), nil
		}
		var encoded string
		var options struct{ Accounts struct{ Addresses []string } }
		if json.Unmarshal(call.Params[0], &encoded) != nil || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("invalid refresh request")
		}
		instructions, err := budgetReserveRefreshInstructions(route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		var keys []publicKey
		for _, address := range options.Accounts.Addresses {
			keys = append(keys, mustKey(address))
		}
		expected, err := encodeLegacyMessage(mustKey(bridgeDelegate), mustKey(bridgeVault), instructions, keys...)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(wire) < 65 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], expected) {
			t.Fatal("refresh is not the closed reserve-only message")
		}
		captures++
		var rows []any
		for _, address := range options.Accounts.Addresses {
			a := accountAt(accounts, address)
			a.Data = append([]byte(nil), a.Data...)
			if address == budgetClockAddress {
				binary.LittleEndian.PutUint64(a.Data[:8], uint64(o.Snapshot.Slot+1))
			}
			if address == route.Kamino.DebtReserve {
				binary.LittleEndian.PutUint64(a.Data[16:24], uint64(o.Snapshot.Slot))
				putScaledFraction(a.Data[296:328], new(big.Int).Add(littleInt(a.Data[296:328]), big.NewInt(123)))
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": o.Snapshot.Slot + 1}, "value": map[string]any{"err": nil, "accounts": rows}}})
		return response(string(payload)), nil
	})
	slot, rawSlot, actual, err := observeAutoPartialRepaymentPrestateSlots(context.Background(), rpc, m, o.Snapshot, o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if captures != 1 || slot != o.Snapshot.Slot+1 || rawSlot != o.Snapshot.Slot || !bytes.Equal(accountAt(actual, route.Kamino.Obligation).Data, rawObligation) || !bytes.Equal(accountAt(actual, route.Kamino.DebtReserve).Data, rawDebtReserve.Data) {
		t.Fatal("refreshed valuation replaced actual principal")
	}
	if accountAt(actual, route.Kamino.DebtReserve).ValuationSource == routeRefreshValuationSource {
		t.Fatal("virtual reserve promoted to raw principal")
	}
}
