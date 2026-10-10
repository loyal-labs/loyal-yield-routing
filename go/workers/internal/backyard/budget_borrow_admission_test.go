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
	"reflect"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Controlled simulation and quotes around actual compilers/valuation. Real
// deployed borrow fee behavior is separately compared in kamino_borrow_test.
func borrowAdmissionFixture(t *testing.T, output uint64, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
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
	_, full, err := confirmedAccounts(context.Background(), rpc, addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	underlying := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": expectedAddresses}}
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
