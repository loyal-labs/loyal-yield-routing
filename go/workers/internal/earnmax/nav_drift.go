package earnmax

// The worker reports NAV when observed and reported values drift past this
// bound; the deposit quote refuses past the same bound.
const (
	NAVDriftToleranceBPS = uint64(50)
	NAVDriftFloorRaw     = uint64(1_000)
)

func NAVDriftOutsideTolerance(observed, reported uint64) bool {
	bound := max(reported*NAVDriftToleranceBPS/10_000, NAVDriftFloorRaw)
	if observed > reported {
		return observed-reported > bound
	}
	return reported-observed > bound
}
