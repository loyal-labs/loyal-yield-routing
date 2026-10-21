package backyard

func kaminoInitializationEffects(r KaminoInitializationRequest) ExpectedEffects {
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
}
