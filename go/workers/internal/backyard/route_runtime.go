package backyard

// RuntimeRoute is the small, typed surface shared by observation, decision,
// and execution: one registry lane's chain facts (route_catalog.go). It is
// not a user-provided route request and has no ranking or fallback behavior.
type RuntimeRoute struct {
	Lane                      string
	Protocol                  string
	CollateralSymbol          string
	DebtSymbol                string
	Kamino                    KaminoObservationConfig
	CollateralCustody         string
	DebtCustody               string
	CollateralLiquiditySupply string
	CollateralReceiptMint     string
	CollateralReceiptSupply   string
	DebtLiquiditySupply       string
	DebtFeeReceiver           string
	CollateralTokenProgram    string
	DebtTokenProgram          string
	CollateralFarm            string
	ObligationCollateralFarm  string
	DebtFarm                  string
	ObligationDebtFarm        string
}

func sharedUSDCDebt(lane string) bool {
	route, err := runtimeRoute(lane)
	return err == nil && route.Kamino.DebtMint == bridgeUSDC && route.DebtCustody == bridgeSquadsATA
}

// USDC debt cash and bridge cash are one account. Never copy that balance into
// DebtIdleRaw: NAV would count it twice. Other debt mints have distinct custody.
func debtCashRaw(s Snapshot) int64 {
	if sharedUSDCDebt(s.RouteLane) {
		if s.DebtIdleRaw != 0 {
			return -1
		}
		return s.SquadsIdleRaw
	}
	return s.DebtIdleRaw
}
