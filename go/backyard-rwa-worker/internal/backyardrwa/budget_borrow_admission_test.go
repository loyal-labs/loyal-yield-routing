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

// Controlled simulation and quotes around actual compilers/valuation. Real
// deployed borrow fee behavior is separately compared in kamino_borrow_test.
func borrowAdmissionFixture(t *testing.T, output uint64, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, _, client, accounts := fundingAdmissionFixture(t, output)
	route := ethenaUSDePYUSD
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1, 1
	clear(accountAt(accounts, route.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	putKey(t, reserve.Data[192:224], route.DebtFeeReceiver)
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1<<52)
	feeAmount := uint64(4)
	if variant == "funded" {
		feeAmount = 0
		binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 0)
		for i := 0; i < 11; i++ {
			binary.LittleEndian.PutUint32(reserve.Data[kaminoReserveConfigOffset+68+i*8:], 0)
		}
	}
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
	accounts = append(accounts, fee)
	_, _, _, _, rpc, _ := debtResidueAdmissionFixture(t, output, accounts...)
	r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, 1000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve}
	effects, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	addresses := append(depositProjectionAddresses(route), route.Kamino.DebtReserve, route.DebtLiquiditySupply, route.DebtFeeReceiver)
	_, full, err := rpc.GetMultipleAccounts(context.Background(), addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoMessage(r)
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
			t.Fatal("invalid borrow simulation")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		var expectedAddresses []any
		for _, a := range addresses {
			expectedAddresses = append(expectedAddresses, a)
		}
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "sigVerify": false, "replaceRecentBlockhash": false, "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": expectedAddresses}}
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) || !reflect.DeepEqual(options, want) {
			t.Fatal("borrow simulation changed exact unsigned wire/options")
		}
		var rows []any
		for _, original := range full {
			a := original
			a.Data = append([]byte(nil), a.Data...)
			switch a.Address {
			case route.Kamino.Obligation:
				putKey(t, a.Data[1208:1240], route.Kamino.DebtReserve)
				copy(a.Data[1240:1272], reserve.Data[296:328])
				putScaledFraction(a.Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(1000+feeAmount), 60))
				if variant == "position" {
					binary.LittleEndian.PutUint64(a.Data[128:136], 100_000_001)
				}
			case route.Kamino.DebtReserve:
				binary.LittleEndian.PutUint64(a.Data[224:232], 1_000_000-1000-feeAmount)
				putScaledFraction(a.Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(1000+feeAmount), 60))
			case route.DebtCustody:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1000)
			case route.DebtLiquiditySupply:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1_000_000-1000-feeAmount)
			case route.DebtFeeReceiver:
				binary.LittleEndian.PutUint64(a.Data[64:72], 5+feeAmount)
				if variant == "fee" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 5)
				}
			case route.CollateralCustody:
				if variant == "collateral" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 0)
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
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"err": failure, "unitsConsumed": 250_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	d := Decision{Action: OpenRouteStep, StrategyKey: route.Lane, AmountRaw: 1, Reason: "collateral_requires_borrow", IdempotencyKey: "borrow-admission"}
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func TestBorrowAdmissionReservesFeeInterestReleaseAndCompleteReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := borrowAdmissionFixture(t, 20_000, "")
	plan, err := observePhase3BorrowAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToDebtStep, ReportNAV, DeleverRouteStep, ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if !reflect.DeepEqual(actions, want) || plan.BorrowProjection == nil || plan.Payoff == nil || plan.Payoff.ObservedDebtRaw != 1004 || plan.Payoff.UpperDebtRaw < 1005 || plan.Payoff.ThroughUnix != 1420 || total != plan.ExitAfterMicros || plan.ValidThroughSlot > 74 || plan.Snapshot != o.Snapshot {
		t.Fatal("borrow omitted fee/interest or complete return", actions, plan.Payoff)
	}
	release, re, _, err := plan.BorrowRelease.decode()
	if err != nil || re.Accounts[1].BeforeRaw != 1 {
		t.Fatal("release lost collateral remainder", err)
	}
	funding, _, _, err := plan.FundingSwap.Input.decode()
	if err != nil || funding.(JupiterSwapRequest).AmountRaw != re.Accounts[1].AfterRaw {
		t.Fatal("funding did not aggregate release and remainder", err)
	}
	withdraw, _, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || withdraw.(KaminoPrimeUSDCRequest).AmountRaw+release.(KaminoPrimeUSDCRequest).AmountRaw != uint64(o.Snapshot.PositionCollateralRaw) {
		t.Fatal("receipt withdrawal is incomplete", err)
	}
	current, ce, message, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || !reflect.DeepEqual(ce, e.ExpectedEffects) {
		t.Fatal("future state replaced current borrow", err)
	}
	if binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72]) != 0 || binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 1 {
		t.Fatal("projection mutated actual custody")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3Borrow(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
	// Final send binds the actual borrow, initial position and the reserved
	// interest horizon; no signer or send RPC exists in this fixture.
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, _ := Phase3IntentDigest(e.Request, plan.Input.Effects)
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: plan.Input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		data   []byte
		offset int
		value  uint64
	}{{accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data, 64, 2}, {accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data, 128, 100_000_001}} {
		before := binary.LittleEndian.Uint64(change.data[change.offset:])
		binary.LittleEndian.PutUint64(change.data[change.offset:], change.value)
		if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
			t.Fatal("changed borrowing prestate/horizon passed final send")
		}
		binary.LittleEndian.PutUint64(change.data[change.offset:], before)
	}
	clock := accountAt(accounts, budgetClockAddress).Data
	binary.LittleEndian.PutUint64(clock[32:40], 1061)
	_, err = validateBorrowAdmissionPrestate(context.Background(), rpc, e.Request, &plan, 42)
	assertBudgetHold(t, err, "borrow_return_interest_window_changed")
	binary.LittleEndian.PutUint64(clock[32:40], 1000)
	reserve := accountAt(accounts, ethenaUSDePYUSD.Kamino.DebtReserve).Data
	binary.LittleEndian.PutUint16(reserve[kaminoReserveConfigOffset+2:], 1)
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
		t.Fatal("increased interest bound passed final send")
	}
	binary.LittleEndian.PutUint16(reserve[kaminoReserveConfigOffset+2:], 0)
	_, err = decodeKaminoPayoffWindow(plan.BorrowProjection.Accounts, ethenaUSDePYUSD, 42, 8)
	assertBudgetHold(t, err, "invalid_payoff_execution_window")
}

func TestBorrowAdmissionRejectsUnsafeProjectionFundingAndPreservesFundedPath(t *testing.T) {
	for _, variant := range []string{"position", "fee", "collateral", "clock", "failed", "underfunded"} {
		t.Run(variant, func(t *testing.T) {
			output := uint64(20_000)
			if variant == "underfunded" {
				output = 2
			}
			o, d, e, m, rpc, client, _ := borrowAdmissionFixture(t, output, variant)
			if _, err := observePhase3BorrowAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("unsafe borrowing admitted")
			}
		})
	}
	o, d, e, m, rpc, client, _ := borrowAdmissionFixture(t, 20_000, "funded")
	plan, err := observePhase3BorrowAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil || plan.BorrowRelease != nil || plan.FundingSwap != nil || len(plan.Exit) != 11 || plan.Payoff.ThroughUnix != 1180 {
		t.Fatal("already funded return introduced an unnecessary release", err)
	}
	_, effects, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || effects.Accounts[1].BeforeRaw != 1 {
		t.Fatal("funded return discarded existing collateral", err)
	}
}
