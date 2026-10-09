package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func debtTopupAllocationFixture(t *testing.T) (Observation, Decision, BridgeExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, _, client, accounts := partialWithdrawalRestoreFixture(t, autoAUTOPYUSD.Lane, int64(autoFixtureDebtRaw))
	reviewed := topupReviewedProgramAccounts(t)
	pin := reviewedTopupKaminoIdentity()
	for _, address := range []string{pin.program, pin.programData} {
		upsertConfirmedAccount(&accounts, accountAt(reviewed, address))
	}
	slot := pin.deploySlot + 1
	binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], uint64(slot))
	for _, address := range []string{autoAUTOPYUSD.Kamino.Obligation, autoAUTOPYUSD.Kamino.CollateralReserve, autoAUTOPYUSD.Kamino.DebtReserve} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[16:24], uint64(slot))
	}
	const amount = int64(10_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], uint64(amount))
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	s := &o.Snapshot
	s.Slot = slot
	s.WithdrawalDemandRaw, s.VoltrStrategyIdleRaw, s.StagedAmountRaw = 0, 0, 0
	s.StageTransient, s.StagedAmountKnown = false, false
	s.PartialWithdrawalOperationID, s.PartialWithdrawalLTVBPS = "", 0
	s.VoltrIdleRaw, s.TopupDepositRoomRaw = amount, amount
	s.PilotTrancheCapLane = autoAUTOPYUSD.Lane
	s.PostMutationNAVRequired = false
	s.PolicyReady, s.ExitBuildable = true, true
	d := Decision{Action: VoltrAllocateToSquads, AmountRaw: amount, StrategyKey: s.RouteLane, Reason: topupAllocationReason, IdempotencyKey: "debt-topup"}
	r := bridgeTestRequest(d.Action, uint64(amount))
	r.Report = BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: uint64(s.StrategyNAVRaw + amount), SnapshotDigest: s.ReportSnapshotDigest}
	effects, _, _, err := bridgeExpectedEffects(d, uint64(amount), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(r.Report.NAVAfterRaw)
	rpc := autoCleanupRPC(t, slot, accounts)
	actualRPC := topupLoanRPC(t, accounts, "")
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if err = json.Unmarshal(body, &call); err != nil {
			return nil, err
		}
		if call.Method == "getMultipleAccounts" {
			var addresses []string
			if err = json.Unmarshal(call.Params[0], &addresses); err != nil {
				return nil, err
			}
			for _, address := range addresses {
				if address == pin.program {
					return actualRPC.client.Transport.RoundTrip(request)
				}
			}
		}
		return inner.RoundTrip(request)
	})
	return o, d, BridgeExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func TestDebtTopupAllocationPricesWholePositionWithoutPayoffAuthority(t *testing.T) {
	o, d, e, m, rpc, client, accounts := debtTopupAllocationFixture(t)
	original := append([]byte(nil), accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data...)
	plan, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	request, effects, _, err := plan.Input.decodeWithManifest(m)
	if err != nil || request != e.Request || !reflect.DeepEqual(effects, e.ExpectedEffects) || plan.Snapshot != o.Snapshot {
		t.Fatal("current allocation changed", err)
	}
	if plan.topupOrigin == nil || !plan.topupOrigin.valid() || plan.Payoff == nil || plan.PayoffRepayment == nil || plan.ExitAfterMicros <= 0 || plan.BorrowProjection != nil {
		t.Fatal("missing immutable origin or debt-aware cost reserve")
	}
	if !bytes.Equal(original, accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data) {
		t.Fatal("pricing mutated actual debt")
	}
	if plan.ValidThroughSlot > o.Snapshot.Slot+adaptorMaxReportAgeSlots {
		t.Fatal("report window widened")
	}
	payoff, _, _, err := plan.PayoffRepayment.decodeWithManifest(m)
	k, ok := payoff.(KaminoPrimeUSDCRequest)
	if err != nil || !ok || k.FullPayoff {
		t.Fatal("cost projection grants payoff authority", err)
	}
	var total int64
	var staged bool
	wantReturn := plan.QuotedExit.EstimatedUpperOutputRaw
	for _, quote := range plan.AdditionalQuotedExits {
		wantReturn += quote.EstimatedUpperOutputRaw
	}
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
		if step.Action == StageSquadsToVoltr && step.Amount == wantReturn {
			staged = true
		}
		if step.Action == SwapUSDCToDebtStep {
			t.Fatal("uninstalled USDC-to-debt edge used")
		}
	}
	if !staged || total != plan.ExitAfterMicros || len(plan.Exit) == 0 || plan.Exit[0].Action != SwapStableToCollateralStep || plan.Exit[0].Amount != uint64(d.AmountRaw) || !topupFullyFundedReturn(plan) {
		t.Fatal("whole exit or allocated cash omitted")
	}
}

func TestDebtTopupAllocationRejectsChangedIntentAndSafetyState(t *testing.T) {
	o, d, e, m, rpc, client, _ := debtTopupAllocationFixture(t)
	for name, mutate := range map[string]func(*Observation, *Decision, *BridgeExecutionEvidence){
		"active tranche": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) {
			o.Snapshot.TopupTranche = &topupTranche{Stage: topupTrancheAllocated}
		},
		"ordinary tranche": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) {
			o.Snapshot.TopupTranche = &topupTranche{Stage: topupTrancheOrdinary}
		},
		"unproven carry": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) {
			o.Snapshot.TopupTranche = &topupTranche{Stage: topupTrancheComplete, CollateralRemainingRaw: 1}
			o.Snapshot.CollateralIdleRaw = 1
			o.Snapshot.PrimeIdleRaw = 1
		},
		"debt value": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.PositionDebtValueRaw = 0 },
		"withdrawal": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"unwind":     func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.Unwind = true },
		"hard risk":  func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.LTVBPS = 6000 },
		"room":       func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.TopupDepositRoomRaw-- },
		"pilot":      func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.PilotActive = false },
		"stale":      func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.Fresh = false },
		"NAV":        func(_ *Observation, _ *Decision, e *BridgeExecutionEvidence) { e.Request.Report.NAVAfterRaw++ },
		"effects":    func(_ *Observation, _ *Decision, e *BridgeExecutionEvidence) { e.ExpectedEffects.Conserved = false },
		"foreign collateral": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) {
			o.Snapshot.CollateralIdleRaw = 1
			o.Snapshot.PrimeIdleRaw = 1
		},
		"debt custody": func(o *Observation, _ *Decision, _ *BridgeExecutionEvidence) { o.Snapshot.DebtIdleRaw = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			bo, bd, be := o, d, e
			mutate(&bo, &bd, &be)
			if _, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, bo, bd, be); err == nil {
				t.Fatal("unsafe allocation accepted")
			}
		})
	}
}

func TestDebtTopupAllocationRejectsPrincipalAndCustodyDriftBeforePricing(t *testing.T) {
	for name, mutate := range map[string]func([]ConfirmedAccount){
		"principal":     func(a []ConfirmedAccount) { accountAt(a, autoAUTOPYUSD.Kamino.Obligation).Data[1296]++ },
		"borrow marker": func(a []ConfirmedAccount) { accountAt(a, autoAUTOPYUSD.Kamino.Obligation).Data[1288]++ },
		"collateral":    func(a []ConfirmedAccount) { accountAt(a, autoAUTOPYUSD.Kamino.Obligation).Data[128]++ },
		"debt custody":  func(a []ConfirmedAccount) { accountAt(a, autoAUTOPYUSD.DebtCustody).Data[64]++ },
		"program":       func(a []ConfirmedAccount) { accountAt(a, reviewedTopupKaminoIdentity().program).Data[4]++ },
	} {
		t.Run(name, func(t *testing.T) {
			o, d, e, m, rpc, client, accounts := debtTopupAllocationFixture(t)
			inner := rpc.client.Transport
			priced := false
			rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var call struct {
					Method string
					Params []json.RawMessage
				}
				if err = json.Unmarshal(body, &call); err != nil {
					return nil, err
				}
				if call.Method == "getFeeForMessage" {
					priced = true
				}
				response, err := inner.RoundTrip(request)
				if call.Method == "getMultipleAccounts" && bytes.Contains(call.Params[1], []byte("finalized")) {
					mutate(accounts)
				}
				return response, err
			})
			if _, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("changed actual origin admitted")
			}
			if priced {
				t.Fatal("pricing started before actual principal/capability validation")
			}
		})
	}
}

func TestDebtTopupAllocationRejectsUnpricedExit(t *testing.T) {
	o, d, e, m, rpc, client, _ := debtTopupAllocationFixture(t)
	m.RuntimeBindings.AutoPolicy = nil
	if _, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
		t.Fatal("allocation admitted without installed whole-position exit policy")
	}
}
