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

// sharedUSDCDebt reports a lane that borrows USDC into the vault's cash.
func sharedUSDCDebt(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.route.Kamino.DebtMint == bridgeUSDC && l.route.DebtTokenProgram == classicTokenProgram
}
