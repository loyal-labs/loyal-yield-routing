package backyard

import (
	"bytes"
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

// Controlled transport, not deployed-program proof. Only the exact unsigned
// initial deposit is simulatable; all signing/send RPCs remain rejected.
func depositAdmissionFixture(t *testing.T, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	return depositAdmissionFixtureForPosition(t, variant, false)
}

func depositAdmissionFixtureForPosition(t *testing.T, variant string, redeposit bool) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
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
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": expectedAddresses}}
		if err != nil || len(wire) < 65 || wire[0] != 1 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) || !reflect.DeepEqual(options, want) {
			t.Fatal("simulation changed wire, signatures, or closed RPC options")
		}
		var rows []any
		for _, address := range addresses {
			a := accountAt(accounts, address)
			a.Data = append([]byte(nil), a.Data...)
			switch address {
			case route.Kamino.Obligation:
				// The deposit adds receipts to whatever the live obligation holds.
				before := binary.LittleEndian.Uint64(a.Data[128:136])
				binary.LittleEndian.PutUint64(a.Data[128:136], before+909_090)
				if variant == "receipts" {
					binary.LittleEndian.PutUint64(a.Data[128:136], before)
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
