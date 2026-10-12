package backyard

// BridgeExecutionEvidence is the complete confirmed input to the exact bridge
// build boundary. The production bridge observer produces it only from the
// enriched snapshot, pinned adaptor config, policy bytes, obligation, and
// custody set.
type BridgeExecutionEvidence struct {
	Request         BridgeBuildRequest
	ExpectedEffects ExpectedEffects
}
