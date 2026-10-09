package backyard

// B2 1.75x exit (docs/plans/b2-175x-multi-cycle-exit-design.md). One release
// at the pilot release ceiling (min(55%, maxLTV-5, hard-5)) frees
// C - D/ceiling; at 1.75x that is ~0.39 of 0.75 debt. A cycle is
// release -> swap -> PARTIAL repay; after at most leverageExitMaxCycles
// cycles one release must cover the payoff, which the existing
// release -> swap -> full payoff legs then finish.
const (
	leverageExitMaxCycles = 2
	// Swap output planning uses a 1% loss floor, like the up-move check.
	leverageExitSwapLossBPS = 100
)
