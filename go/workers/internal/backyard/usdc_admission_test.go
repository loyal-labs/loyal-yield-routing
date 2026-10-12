package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Reuse the existing complete-return fixture, changing only the route's concrete
// identities and token program. Real compilers/admission run against controlled
// transport; no simulation, signer or live-program success is claimed here.
func usdcReturnFixture(t *testing.T) (Observation, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	return usdcReturnFixtureForLane(t, SelectedRouteID)
}

// usdcReturnFixtureForLane maps the controlled Ethena fixture onto one basic
// USDC-debt lane (Maple or OnRe) and its recorded Jupiter exports.
func usdcReturnFixtureForLane(t *testing.T, lane string) (Observation, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	old := ethenaUSDePYUSD
	route, _ := runtimeRoute(lane)
	symbol := route.CollateralSymbol
	o, _, _, m, oldRPC, _, existing := payoffAdmissionFixture(t, 20_000)
	addresses := []string{old.Kamino.CollateralReserve, old.Kamino.CollateralMint, old.Kamino.DebtMint, reportTicketPDA}
	_, extra, err := confirmedAccounts(context.Background(), oldRPC, addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	existing = append(existing, extra...)
	pairs := [][2]string{
		{old.Kamino.Market, route.Kamino.Market}, {old.Kamino.MarketAuthority, route.Kamino.MarketAuthority},
		{old.Kamino.Obligation, route.Kamino.Obligation}, {old.Kamino.CollateralReserve, route.Kamino.CollateralReserve},
		{old.Kamino.DebtReserve, route.Kamino.DebtReserve}, {old.Kamino.CollateralMint, route.Kamino.CollateralMint},
		{old.Kamino.DebtMint, route.Kamino.DebtMint}, {old.CollateralCustody, route.CollateralCustody},
		{old.DebtCustody, route.DebtCustody}, {old.CollateralLiquiditySupply, route.CollateralLiquiditySupply},
		{old.DebtLiquiditySupply, route.DebtLiquiditySupply}, {old.DebtFeeReceiver, route.DebtFeeReceiver},
		{old.CollateralReceiptMint, route.CollateralReceiptMint}, {old.CollateralReceiptSupply, route.CollateralReceiptSupply},
	}
	// The payoff fixture already carries its lane's market, renamed below.
	var accounts []ConfirmedAccount
	for _, a := range existing {
		a.Data = append([]byte(nil), a.Data...)
		for _, p := range pairs {
			if a.Address == p[0] {
				a.Address = p[1]
			}
			from, e1 := decodeKey(p[0])
			to, e2 := decodeKey(p[1])
			if e1 == nil && e2 == nil {
				a.Data = bytes.ReplaceAll(a.Data, from[:], to[:])
			}
		}
		if a.Address == route.DebtCustody || a.Address == route.DebtLiquiditySupply || a.Address == bridgeUSDC {
			a.Owner = classicTokenProgram
		}
		accounts = append(accounts, a)
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	raw, err := os.ReadFile(jupiterV2FixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var headers struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	if err = json.Unmarshal(raw, &headers); err != nil {
		t.Fatal(err)
	}
	var instruction JupiterSwapInstruction
	client, _ := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" {
			q := req.URL.Query()
			amount, err := strconv.ParseUint(q.Get("amount"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			key, out := symbol+"->USDC", amount/1000
			if q.Get("inputMint") == bridgeUSDC {
				key, out = "USDC->"+symbol, amount*1000
			}
			for _, row := range headers.Rows {
				if row.Key == key {
					instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
				}
			}
			wire, err := base64.StdEncoding.DecodeString(instruction.Data)
			if err != nil || len(wire) < jupiter.V2RoutePlanOffset {
				t.Fatal("missing basic quote")
			}
			binary.LittleEndian.PutUint64(wire[jupiter.V2InAmountOffset:], amount)
			binary.LittleEndian.PutUint64(wire[jupiter.V2QuotedOutOffset:], out)
			binary.LittleEndian.PutUint16(wire[jupiter.V2SlippageOffset:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(wire)
			payload = jupiter.Quote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 9950 / 10000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction}
		}
		data, _ := json.Marshal(payload)
		return response(string(data)), nil
	}))
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = route.Lane, route.Lane
	o.Snapshot.SquadsIdleRaw, o.Snapshot.DebtIdleRaw = 11_000, 0
	o.Snapshot.PositionDebtValueRaw = 1000
	return o, m, rpc, client, accounts
}

func TestUSDCHardLTVIsOneDownStep(t *testing.T) {
	t.Parallel()
	s := base()
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw, s.PayoffDebtRaw, s.LTVBPS = true, 200, 100, 101, 6000
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 200, 100
	for _, lane := range append(basicLaneIDs(), SelectedRouteID) {
		s.RouteLane, s.StrategyKey = lane, lane
		// Whatever cash or collateral sits idle, the step funds its own repay.
		for _, cash := range []int64{0, 99, 101} {
			s.SquadsIdleRaw, s.CollateralIdleRaw, s.CollateralIdleValueRaw = cash, cash, cash
			d := Decide(s)
			if d.Action != DeleverRouteStep || d.Reason != hardLTVDeleverReason || d.Validate() != nil {
				t.Fatalf("%s hard LTV with %d idle: %+v (%v)", lane, cash, d, d.Validate())
			}
		}
	}
}
