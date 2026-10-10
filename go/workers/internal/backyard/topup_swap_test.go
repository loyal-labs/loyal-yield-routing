package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Plan B3 leg 2 fixture: a funded debt-free position with 20,000 raw bridge
// USDC in Squads (the top-up tranche).
func topupSwapAdmissionFixture(t *testing.T) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	clear(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw, o.Snapshot.PayoffDebtRaw = 0, 0, 0, 0
	d := Decision{Action: SwapStableToCollateralStep, AmountRaw: 20_000, StrategyKey: o.Snapshot.RouteLane, Reason: topupSwapReason, IdempotencyKey: "topup-swap"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 20_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
	return o, d, e, m, rpc, client, accounts
}

// Plan B3 leg 2 beside debt (AUTO): $40 of Squads cash next to the PYUSD
// loan. The unsigned swap simulation spends the Squads cash and credits the
// quoted AUTO; variant breaks one fact of that poststate.
func autoDebtTopupSwapFixture(t *testing.T, variant string) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	o, m, rpc, client, accounts := autoDebtTopupFixture(t)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 40_000_000)
	o.Snapshot.SquadsIdleRaw = 40_000_000
	d := Decision{Action: SwapStableToCollateralStep, AmountRaw: 40_000_000, StrategyKey: o.Snapshot.RouteLane, Reason: topupSwapReason, IdempotencyKey: "topup-swap-debt"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 40_000_000, 0, o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
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
		var options struct{ Accounts struct{ Addresses []string } }
		if len(call.Params) != 2 || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("invalid swap simulation")
		}
		var rows []any
		for _, address := range options.Accounts.Addresses {
			a := accountAt(accounts, address)
			a.Data = append([]byte(nil), a.Data...)
			switch address {
			case bridgeSquadsATA:
				cash := uint64(0)
				if variant == "source" {
					cash = 1
				}
				binary.LittleEndian.PutUint64(a.Data[64:72], cash)
			case autoAUTOPYUSD.CollateralCustody:
				cash := e.Request.QuotedOutputRaw
				if variant == "output" {
					cash = e.Request.MinimumOutputRaw - 1
				}
				binary.LittleEndian.PutUint64(a.Data[64:72], cash)
			case autoAUTOPYUSD.Kamino.Obligation:
				if variant == "position" {
					a.Data[128] ^= 1
				}
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": o.Snapshot.Slot}, "value": map[string]any{"err": nil, "unitsConsumed": 200_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	return o, d, e, m, rpc, client
}

// Beside debt, build and send re-read the debt-bearing obligation for the
// flagged top-up swap on AUTO only.
func TestTopupSwapBesideDebtPrestateAcceptsOnlyTheFlaggedTopup(t *testing.T) {
	_, _, e, m, rpc, _ := autoDebtTopupSwapFixture(t, "")
	if _, err := m.validateRequestPrestate(context.Background(), rpc, e.Request, e.ExpectedEffects); err != nil {
		t.Fatal("prestate refused the top-up swap beside debt", err)
	}
	flat := e
	flat.Request.TopupReturnReserved = false
	if _, err := m.validateRequestPrestate(context.Background(), rpc, flat.Request, flat.ExpectedEffects); err == nil {
		t.Fatal("unflagged entry swap accepted a debt-bearing obligation")
	}
}

func TestTopupSwapDecisionBesideDebtFreePosition(t *testing.T) {
	s := liveIdleDebtSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.DebtIdleRaw = 0, 0, 0, 0, 0
	s.SquadsIdleRaw = 336_000_000
	got := Decide(s)
	if got.Action != SwapStableToCollateralStep || got.Reason != topupSwapReason || got.AmountRaw != s.SquadsIdleRaw || got.Validate() != nil {
		t.Fatalf("top-up cash was not swapped: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw, s.VoltrIdleRaw = 10, 0 },
		"unwind":           func(s *Snapshot) { s.Unwind = true },
		"hard ltv":         func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 1, 1, 6000 },
		"report due":       func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupSwapReason {
			t.Fatalf("%s lost priority to the top-up swap: %+v", name, got)
		}
	}
}

// Build and send accept the flagged top-up swap beside the debt-free
// position; a flat entry swap may not carry it.
func TestTopupSwapPrestateAcceptsTheFlaggedPosition(t *testing.T) {
	_, _, e, _, rpc, _, _ := topupSwapAdmissionFixture(t)
	if err := validateBuildPrestate(context.Background(), rpc, e.Request, e.ExpectedEffects); err != nil {
		t.Fatal("prestate refused the top-up swap", err)
	}
	flat := e
	flat.Request.TopupReturnReserved = false
	if err := validateBuildPrestate(context.Background(), rpc, flat.Request, flat.ExpectedEffects); err == nil {
		t.Fatal("entry swap without the top-up flag accepted a funded obligation")
	}
}
