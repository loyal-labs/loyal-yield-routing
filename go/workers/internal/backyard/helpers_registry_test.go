package backyard

// Test shorthands over the registry's Prime/PRIME/USDC lane, the PRIME/USDC
// graph most fixtures are built on.
func pinnedKaminoObservationConfig() (KaminoObservationConfig, error) {
	return primePRIMEUSDC.Kamino, nil
}

func ComputeRouteNAV(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{primePRIMEUSDC})
}

func ComputeRouteNAVForRoute(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, route RuntimeRoute) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{route})
}
