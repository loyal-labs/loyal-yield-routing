package backyard

// These values contain no receipt count or account image. Callers supply a
// conservative underlying collateral amount and a finite-window debt bound.
type kaminoReleaseValues struct {
	CollateralRaw      uint64
	DebtRaw            uint64
	CollateralDecimals uint8
	DebtDecimals       uint8
	CollateralPriceSF  [16]byte
	DebtPriceSF        [16]byte
}

func releaseValuesForPosition(position KaminoPosition) kaminoReleaseValues {
	return kaminoReleaseValues{CollateralRaw: position.RedeemablePrimeRaw, DebtRaw: position.DebtRaw,
		CollateralDecimals: position.CollateralDecimals, DebtDecimals: position.DebtDecimals,
		CollateralPriceSF: position.CollateralPriceSF, DebtPriceSF: position.DebtPriceSF}
}
