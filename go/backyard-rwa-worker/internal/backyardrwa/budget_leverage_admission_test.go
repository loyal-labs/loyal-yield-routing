package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"testing"
)

func leverageAdmissionFixture(t *testing.T, output uint64, variant string) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := fundingAdmissionFixture(t, output)
	route := ethenaUSDePYUSD
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 1004, 2008, 1000
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.CollateralIdleValueRaw = 1, 1, 0
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(1004), 60))
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
	d := Decision{Action: SwapDebtToCollateralStep, StrategyKey: route.Lane, AmountRaw: 1000, Reason: "borrowed_debt_requires_collateral_buffer", IdempotencyKey: "leverage-admission"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 1000, 1, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.PositionReturnReserved = true
	_, before, err := observeKaminoPayoffWindow(context.Background(), rpc, route, 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileJupiterMessage(e.Request)
	if err != nil {
		t.Fatal(err)
	}
	underlying := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(body, &call) != nil {
			t.Fatal("invalid RPC")
		}
		if call.Method != "simulateTransaction" {
			return underlying.RoundTrip(req)
		}
		var encoded string
		var options map[string]any
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &encoded) != nil || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("invalid swap simulation")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		var addresses []any
		for _, a := range before {
			addresses = append(addresses, a.Address)
		}
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "sigVerify": false, "replaceRecentBlockhash": false, "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": addresses}}
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) || !reflect.DeepEqual(options, want) {
			t.Fatal("changed unsigned simulation wire/options")
		}
		var rows []any
		for _, original := range before {
			a := original
			a.Data = append([]byte(nil), a.Data...)
			switch a.Address {
			case route.DebtCustody:
				cash := uint64(0)
				if variant == "source" {
					cash = 1
				}
				binary.LittleEndian.PutUint64(a.Data[64:72], cash)
			case route.CollateralCustody:
				cash := uint64(1) + e.Request.QuotedOutputRaw
				if variant == "output" {
					cash = e.ExpectedEffects.Accounts[1].AfterRaw - 1
				}
				binary.LittleEndian.PutUint64(a.Data[64:72], cash)
			case route.Kamino.Obligation:
				if variant == "position" {
					binary.LittleEndian.PutUint64(a.Data[128:136], 100_000_001)
				}
			case route.Kamino.CollateralReserve:
				if variant == "reserve" {
					a.Data[224] ^= 1
				}
			case budgetClockAddress:
				if variant == "clock" {
					binary.LittleEndian.PutUint64(a.Data[:8], 41)
				}
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		var failure any
		if variant == "failed" {
			failure = "invalid"
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"err": failure, "unitsConsumed": 200_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	return o, d, e, m, rpc, client, accounts
}

func TestLeverageSwapAdmissionReservesProjectedCompleteReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := leverageAdmissionFixture(t, 20_000, "")
	plan, err := observePhase3LeverageSwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LeverageProjection == nil || plan.BorrowProjection != nil || plan.BorrowRelease != nil || plan.FundingRelease == nil || plan.FundingSwap == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil || len(plan.Exit) != 17 || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1420 || plan.ValidThroughSlot > 74 {
		t.Fatal("leverage swap omitted complete position return")
	}
	current, _, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || plan.Snapshot != o.Snapshot {
		t.Fatal("exit projection replaced current swap", err)
	}
	_, releaseEffects, _, err := plan.FundingRelease.decode()
	if err != nil || releaseEffects.Accounts[1].BeforeRaw != 1+e.Request.QuotedOutputRaw {
		t.Fatal("exit lost simulated swap proceeds/remainder", err)
	}
	var total int64
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
	}
	if total != plan.ExitAfterMicros || binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72]) != 1000 || binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 1 {
		t.Fatal("exit costing changed observed custody or omitted costs")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3LeverageSwap(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
	encoded, _ := jsonMarshalExpectedEffects(e.ExpectedEffects)
	input, _ := encodePhase3BuildInput(e.Request, encoded)
	intent, _ := Phase3IntentDigest(e.Request, encoded)
	message, _ := CompileJupiterMessage(e.Request)
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 2)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "leverage_swap_custody_changed")
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 1)
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[128:136], 100_000_001)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "leverage_swap_snapshot_changed")
}

func TestLeverageSwapAdmissionRejectsUnsafePoststateFundingAndIntent(t *testing.T) {
	for _, variant := range []string{"source", "output", "position", "reserve", "clock", "failed", "underfunded", "overcap"} {
		t.Run(variant, func(t *testing.T) {
			output := uint64(20_000)
			if variant == "underfunded" {
				output = 1000
			}
			if variant == "overcap" {
				output = 900_000
			}
			o, d, e, m, rpc, client, _ := leverageAdmissionFixture(t, output, variant)
			_, err := observePhase3LeverageSwapAdmission(context.Background(), rpc, client, m, o, d, e)
			if err == nil {
				t.Fatal("unsafe leverage admitted")
			}
			if variant == "underfunded" {
				assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
			}
			if variant == "overcap" {
				assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
			}
		})
	}
	o, d, e, m, rpc, client, _ := leverageAdmissionFixture(t, 20_000, "")
	e.Request.PositionReturnReserved = false
	_, err := observePhase3LeverageSwapAdmission(context.Background(), rpc, client, m, o, d, e)
	assertBudgetHold(t, err, "leverage_swap_intent_mismatch")
	e.Request.PositionReturnReserved, e.Request.EntryReturnReserved = true, true
	_, err = MeasureExecutableDebit(e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "invalid_position_return_intent")
	e.Request.EntryReturnReserved, o.Snapshot.CutoverDrain = false, true
	_, err = observePhase3LeverageSwapAdmission(context.Background(), rpc, client, m, o, d, e)
	assertBudgetHold(t, err, "complete_leverage_swap_return_unavailable")
}
