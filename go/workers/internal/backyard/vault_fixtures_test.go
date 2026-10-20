package backyard

// The rwa vault's identities and routes, by the names the tests use.
var (
	bridgeVault           = rwa.squads
	bridgeSquadsATA       = rwa.cash
	bridgeVoltrVault      = rwa.VoltrVault
	bridgeStrategy        = rwa.Strategy
	bridgeStrategyReceipt = rwa.strategyReceipt
	bridgeIdleATA         = rwa.idleATA
	bridgeIdleAuthority   = rwa.idleAuth
	bridgeStrategyATA     = rwa.strategyATA
	bridgeStrategyAuth    = rwa.strategyAuth
	bridgeLPMint          = rwa.lpMint
	bridgeAdaptorProgram  = rwa.Adaptor
	productionRouteKey    = rwa.RouteKey

	mapleSyrupUSDCUSDC = rwaRoute(mapleSyrupUSDCUSDCTemplate.Lane)
	onreONycUSDCRoute  = rwaRoute(onreONycUSDCRouteTemplate.Lane)
	primePRIMEUSDC     = rwaRoute(primePRIMEUSDCTemplate.Lane)
	primePRIMEPYUSD    = rwaRoute(primePRIMEPYUSDTemplate.Lane)
	primePRIMEUSDS     = rwaRoute(primePRIMEUSDSTemplate.Lane)
	autoAUTOPYUSD      = rwaRoute(autoAUTOPYUSDTemplate.Lane)
	autoAUTOWYLDS      = rwaRoute(autoAUTOWYLDSTemplate.Lane)
	ethenaUSDePYUSD    = rwaRoute(ethenaUSDePYUSDTemplate.Lane)
	pyusdCustody       = autoAUTOPYUSD.DebtCustody
)

func rwaRoute(lane string) RuntimeRoute {
	route, err := rwa.route(lane)
	if err != nil {
		panic(err)
	}
	return route
}

func runtimeRoute(lane string) (RuntimeRoute, error) { return rwa.route(lane) }

func loadEmbeddedRouteManifest() (RouteManifest, error) { return rwa.manifest, nil }

// testVaultState is the rwa worker's in-process state the tests observe
// through, and its two streaks by their names.
var (
	testVaultState            = &vaultState{}
	refreshSimulationFailures = &testVaultState.refreshFailures
	kaminoStaleSince          = &testVaultState.staleSince
)
