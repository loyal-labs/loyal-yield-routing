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

func leverageAdmissionFixture(t *testing.T, output uint64, variant string) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
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
			t.Fatal("invalid swap simulation")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		var addresses []any
		for _, a := range before {
			addresses = append(addresses, a.Address)
		}
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": addresses}}
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

// The build and final-send prestate of a leverage swap: the persisted swap
// passes against its own custody; a changed debt buffer refuses, and a request
// that also claims entry-return authority is refused by the effect graph.
func TestLeverageSwapPrestateBindsCustodyAndIntent(t *testing.T) {
	_, _, e, _, rpc, _, accounts := leverageAdmissionFixture(t, 20_000, "")
	if err := validateBuildPrestate(context.Background(), rpc, e.Request, e.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 999)
	assertBudgetHold(t, validateBuildPrestate(context.Background(), rpc, e.Request, e.ExpectedEffects), "leverage_swap_custody_changed")
	e.Request.EntryReturnReserved = true
	_, err := MeasureExecutableDebit(e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "invalid_position_return_intent")
}
