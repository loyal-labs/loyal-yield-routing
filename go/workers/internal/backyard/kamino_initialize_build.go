package backyard

// kaminoInitializationEffects is the initializer's whole effect graph: it
// creates the obligation and moves no token.
func kaminoInitializationEffects(r KaminoInitializationRequest) ExpectedEffects {
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
}
