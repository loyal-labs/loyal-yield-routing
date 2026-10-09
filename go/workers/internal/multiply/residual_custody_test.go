package multiply

import (
	"context"
	"errors"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/solana-foundation/solana-go/v2"
)

type residualQuoteFixture struct {
	topology *EarnMaxTopology
	request  *jupiter.QuoteRequest
	deny     bool
}

func (f *residualQuoteFixture) Quote(ctx contextT, request jupiter.QuoteRequest) (jupiter.Quote, error) {
	copy := request
	f.request = &copy
	if f.deny {
		return jupiter.Quote{}, errors.New("no executable market route")
	}
	return (fakeQuoteClient{f.topology}).Quote(ctx, request)
}
func (f *residualQuoteFixture) SwapInstructions(ctx contextT, quote jupiter.Quote, vault solana.PublicKey, useSharedAccounts bool) (jupiter.SwapInstructions, error) {
	response, err := (fakeQuoteClient{f.topology}).SwapInstructions(ctx, quote, vault, useSharedAccounts)
	if err != nil {
		return jupiter.SwapInstructions{}, err
	}
	config := f.topology.Strategies[SyrupUsdcPyusd]
	response.SwapInstruction.Accounts[3].Pubkey = config.DebtCustody.String()
	response.SwapInstruction.Accounts[6].Pubkey = config.CollateralCustody.String()
	response.SwapInstruction.Accounts[7].Pubkey = quote.InputMint
	response.SwapInstruction.Accounts[8].Pubkey = quote.OutputMint
	return response, nil
}

func TestWithdrawalResidualDebtCleanupPreservesAliasedClaimAndExactAmount(t *testing.T) {
	topology := testTopology(t)
	route := testRouteState(t, topology)
	route.Goal = GoalWithdraw
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 10_000_000
	for index := range observed.DebtCustodies {
		if observed.DebtCustodies[index].Balance.Account == observed.Claim.Account {
			observed.DebtCustodies[index].Balance.AmountRaw = observed.Claim.AmountRaw
		}
	}
	if decision := NextAction(route, observed, topology); decision.Kind != "complete" {
		t.Fatalf("USDC claim alias was spent twice: %+v", decision)
	}
	observed.DebtCustody(SyrupUsdcPyusd).AmountRaw = 250_001
	decision := NextAction(route, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionSwapDebtToCollateral || decision.Plan.StrategyKey != SyrupUsdcPyusd || decision.Plan.Amount.Exactly != 250_001 {
		t.Fatalf("orphan borrowed custody disappeared: %+v", decision)
	}
	quotes := &residualQuoteFixture{topology: topology}
	built, err := BuildOperation(decision.Plan, observed, topology, quotes, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	config := topology.Strategies[SyrupUsdcPyusd]
	if quotes.request.InputMint != PYUSDMint || quotes.request.Amount != 250_001 || built.ExpectedEffects.TokenDeltas[0].Account != config.DebtCustody.String() || built.ExpectedEffects.TokenDeltas[0].RawDelta != -250_001 {
		t.Fatal("residual quote changed borrowed mint, custody or exact input")
	}
	observed.DebtCustody(SyrupUsdcPyusd).AmountRaw = 0
	observed.CollateralCustody(SyrupUsdcPyusd).AmountRaw = 246_263
	decision = NextAction(route, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionSwapCollateralToClaim || decision.Plan.Amount.Exactly != 246_263 {
		t.Fatalf("residual collateral never returned to claim custody: %+v", decision)
	}
}

func TestResidualDebtDustQuoteAndPermissionFailuresNeverPublishOperation(t *testing.T) {
	topology := testTopology(t)
	route := testRouteState(t, topology)
	route.Goal = GoalWithdraw
	observed := idleObserved(topology)
	observed.DebtCustody(SyrupUsdcPyusd).AmountRaw = 50_000
	decision := NextAction(route, observed, topology)
	if decision.Kind != "unresolved_custody" || decision.Condition != "withdrawal_residual_debt_dust" {
		t.Fatal("unexecutable debt dust became complete")
	}
	observed.DebtCustody(SyrupUsdcPyusd).AmountRaw = 250_001
	decision = NextAction(route, observed, topology)
	executor, _, _ := testExecutor(t)
	for _, test := range []struct {
		deny      bool
		condition string
	}{{true, "withdrawal_residual_debt_quote_unavailable"}, {false, "withdrawal_residual_debt_policy_unavailable"}} {
		// Deliberately no Store: quote and permission rejection must happen before
		// any preparation, immutable wire publication or executor broadcast.
		worker := &Worker{executor: executor, quotes: &residualQuoteFixture{topology: topology, deny: test.deny}}
		result, err := worker.executePlan(context.Background(), nil, &StoredRoute{RouteKey: route.RouteKey, State: route}, topology, observed, decision.Plan)
		if err != nil || result.Condition != test.condition {
			t.Fatalf("unexecutable cleanup %v %v", result, err)
		}
		if route.CurrentOperationID != nil || observed.DebtCustody(SyrupUsdcPyusd).AmountRaw != 250_001 {
			t.Fatal("blocked cleanup changed owned custody")
		}
	}
}
