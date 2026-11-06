package earnmax

// Each bound is in bps of the reference value and never below NAVDriftFloorRaw.
const (
	// NAVPublicationBandBPS: the worker publishes when net NAV and the
	// published value differ by more, and claims wait past it.
	NAVPublicationBandBPS = uint64(10)
	// NAVAnomalyBPS: gross drift past it with no capital mutation is
	// unexplained, and the worker latches instead of publishing.
	NAVAnomalyBPS = uint64(50)
	// DepositOverpayBPS: a deposit waits while the published value exceeds
	// certified NAV by more. The holder side is bounded by the issuance fee.
	DepositOverpayBPS = uint64(50)
	NAVDriftFloorRaw  = uint64(1_000)
)

// NAVOutside reports whether value and reference differ by more than bps of reference.
func NAVOutside(value, reference, bps uint64) bool {
	bound := max(reference*bps/10_000, NAVDriftFloorRaw)
	if value > reference {
		return value-reference > bound
	}
	return reference-value > bound
}

// NAVExceeds is NAVOutside with value above reference.
func NAVExceeds(value, reference, bps uint64) bool {
	return value > reference && NAVOutside(value, reference, bps)
}
