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

// Controlled transport, not deployed-program proof. Only the exact unsigned
// initial deposit is simulatable; all signing/send RPCs remain rejected.
func depositAdmissionFixture(t *testing.T, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	return depositAdmissionFixtureForPosition(t, variant, false)
}

func depositAdmissionFixtureForPosition(t *testing.T, variant string, redeposit bool) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, _, client, accounts := fundingAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	obligation := accountAt(accounts, route.Kamino.Obligation)
	if redeposit {
		o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1_000_000, 1_000_000
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1_000_000)
	} else {
		o.Snapshot.HasPosition = false
		o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 0, 0
		o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw = 0, 0
		binary.LittleEndian.PutUint64(obligation.Data[128:136], 0)
		clear(obligation.Data[1208:1408])
	}
	o.Snapshot.DebtIdleRaw = 0
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_100_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	binary.LittleEndian.PutUint32(reserve.Data[28:32], 1000)
	reserve.Data[kaminoReserveConfigOffset+9] = 1
	for i := 0; i < 11; i++ {
		offset := kaminoReserveConfigOffset + 64 + i*8
		if i > 0 {
			binary.LittleEndian.PutUint32(reserve.Data[offset:], 10_000)
		}
		binary.LittleEndian.PutUint32(reserve.Data[offset+4:], 7500)
	}
	accounts = append(accounts, reserve)
	_, _, _, _, rpc, _ := withdrawalAdmissionFixture(t, 20_000, accounts...)
	if redeposit {
		_, _, _, _, rpc, _ = debtResidueAdmissionFixture(t, 20_000, accounts...)
	}
	r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	if redeposit {
		r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	}
	effects, err := boundedKaminoDepositEffects(accounts, route, 42, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: OpenRouteStep, AmountRaw: int64(r.AmountRaw), StrategyKey: route.Lane, Reason: "deposit", IdempotencyKey: "deposit-admission"}
	if redeposit {
		d.Reason = "single_loop_redeposit"
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
			t.Fatal("invalid RPC request")
		}
		if call.Method != "simulateTransaction" {
			return underlying.RoundTrip(req)
		}
		var encoded string
		var options map[string]any
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &encoded) != nil || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("invalid simulation")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		addresses := depositProjectionAddresses(route)
		if redeposit {
			addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply)
		}
		var expectedAddresses []any
		for _, address := range addresses {
			expectedAddresses = append(expectedAddresses, address)
		}
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "sigVerify": false, "replaceRecentBlockhash": false, "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": expectedAddresses}}
		if err != nil || len(wire) < 65 || wire[0] != 1 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) || !reflect.DeepEqual(options, want) {
			t.Fatal("simulation changed wire, signatures, or closed RPC options")
		}
		var rows []any
		for _, address := range addresses {
			a := accountAt(accounts, address)
			a.Data = append([]byte(nil), a.Data...)
			switch address {
			case route.Kamino.Obligation:
				binary.LittleEndian.PutUint64(a.Data[128:136], uint64(o.Snapshot.PositionCollateralRaw)+909_090)
				if variant == "receipts" {
					binary.LittleEndian.PutUint64(a.Data[128:136], uint64(o.Snapshot.PositionCollateralRaw))
				}
				if variant == "debt" {
					putScaledFraction(a.Data[1296:1312], new(big.Int).Lsh(big.NewInt(1001), 60))
				}
			case route.Kamino.CollateralReserve:
				binary.LittleEndian.PutUint64(a.Data[224:232], 1_100_999_999)
				binary.LittleEndian.PutUint64(a.Data[2592:2600], 1_000_909_090)
			case route.CollateralCustody:
				binary.LittleEndian.PutUint64(a.Data[64:72], uint64(o.Snapshot.CollateralIdleRaw)-999_999)
			case route.DebtCustody:
				if variant == "debt_cash" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 1)
				}
			case route.CollateralLiquiditySupply:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1_000_999_999)
				if variant == "conservation" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 1_001_000_000)
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
			failure = map[string]any{"InstructionError": []any{0, "Custom"}}
		}
		if variant == "missing" {
			rows[0] = nil
		}
		slot := 42
		if variant == "stale" {
			slot = 75
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": slot}, "value": map[string]any{"err": failure, "unitsConsumed": 200_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func TestDepositAdmissionReservesWithdrawalResidueAndCompleteBridgeReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := depositAdmissionFixture(t, "")
	plan, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	want := []Action{ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.DepositProjection == nil || plan.PayoffWithdrawal == nil || plan.Snapshot != o.Snapshot {
		t.Fatal("incomplete deposit reserve")
	}
	current, effects, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || !reflect.DeepEqual(effects, e.ExpectedEffects) {
		t.Fatal("simulation replaced current instruction", err)
	}
	withdraw, we, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || withdraw.(KaminoPrimeUSDCRequest).AmountRaw != 909_090 || we.Accounts[1].BeforeRaw != 19_000_001 {
		t.Fatal("withdrawal lost actual receipt count or residue", err)
	}
	reverse, _, _, err := plan.QuotedExit.Input.decode()
	if err != nil || reverse.(JupiterSwapRequest).AmountRaw != we.Accounts[1].AfterRaw || plan.Exit[5].Amount != plan.QuotedExit.EstimatedUpperOutputRaw || plan.Exit[7].Amount != plan.Exit[5].Amount {
		t.Fatal("return did not price full withdrawal plus residue", err)
	}
	if binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 20_000_000 {
		t.Fatal("projection mutated live inputs")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3Deposit(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
	// The final-send path uses the exact persisted deposit, rechecks custody
	// and empty position, and never signs or broadcasts in this test.
	_, _, message, err := plan.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, _ := Phase3IntentDigest(e.Request, plan.Input.Effects)
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: plan.Input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		address  string
		offset   int
		original uint64
	}{
		{ethenaUSDePYUSD.CollateralCustody, 64, 20_000_000},
		{ethenaUSDePYUSD.DebtCustody, 64, 0},
		{ethenaUSDePYUSD.Kamino.Obligation, 128, 0},
	} {
		data := accountAt(accounts, change.address).Data
		binary.LittleEndian.PutUint64(data[change.offset:], change.original+1)
		if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
			t.Fatal("final-send accepted changed deposit prestate", change.address)
		}
		binary.LittleEndian.PutUint64(data[change.offset:], change.original)
	}
	// Reobserve an actual deposit poststate for NAV; never promote a template.
	postAccounts := append(append([]ConfirmedAccount(nil), accounts...), plan.DepositProjection.Accounts...)
	_, _, _, _, postRPC, _ := withdrawalAdmissionFixture(t, 20_000, postAccounts...)
	post := o
	post.Snapshot.HasPosition = true
	post.Snapshot.PositionCollateralRaw = 909_090
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = 19_000_001, 19_000_001
	nav := bridgeTestRequest(ReportNAV, 0)
	nav.Report.Sequence, nav.Report.ObservedSlot = 42, 42
	nd := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	ne, _, _, err := bridgeExpectedEffects(nd, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := pricePhase3PositionReturn(context.Background(), postRPC, client, m, post, nd, nav, ne, false)
	if err != nil || len(continuation.Exit) != 8 {
		t.Fatal("post-deposit NAV lost return", err)
	}
	quoted, _, _, err := continuation.QuotedExit.Input.decode()
	if err != nil || quoted.(JupiterSwapRequest).AmountRaw != reverse.(JupiterSwapRequest).AmountRaw {
		t.Fatal("post-deposit residue omitted", err)
	}
}

func TestDepositAdmissionRejectsFailedProjectionAndChangedCustody(t *testing.T) {
	for _, variant := range []string{"failed", "missing", "stale", "clock", "conservation", "position", "debt", "source"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, client, accounts := depositAdmissionFixture(t, variant)
			switch variant {
			case "position":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[128:136], 1)
			case "debt":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 1)
			case "source":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 20_000_001)
			}
			if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("unsafe deposit admitted")
			}
		})
	}
}
