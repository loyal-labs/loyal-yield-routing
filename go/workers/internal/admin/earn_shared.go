package admin

import (
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

const earnStablecoinDecimals = 6

type EarnStablecoin struct {
	Decimals int    `json:"decimals"`
	Mint     string `json:"mint"`
	Symbol   string `json:"symbol"`
}

var EarnStablecoins = func() []EarnStablecoin {
	products := earncatalog.Mainnet()
	coins := make([]EarnStablecoin, len(products))
	for i, product := range products {
		coins[i] = EarnStablecoin{earnStablecoinDecimals, product.Mint, product.Symbol}
	}
	return coins
}()

var earnSafeRiskBasketMarkets = []string{
	"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
	"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
	"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
	"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
	"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
}

func EarnStablecoinByMint(mint string) (EarnStablecoin, bool) {
	for _, coin := range EarnStablecoins {
		if coin.Mint == mint {
			return coin, true
		}
	}
	return EarnStablecoin{}, false
}

func EarnStablecoinBySymbol(symbol string) (EarnStablecoin, bool) {
	for _, coin := range EarnStablecoins {
		if coin.Symbol == symbol {
			return coin, true
		}
	}
	return EarnStablecoin{}, false
}

func EarnStablecoinSymbol(mint string) (string, bool) {
	coin, ok := EarnStablecoinByMint(mint)
	return coin.Symbol, ok
}

type StablecoinHealthWarning struct {
	Code    string `json:"code" enum:"no_eligible_reserve"`
	Level   string `json:"level" enum:"info,warning,critical"`
	Message string `json:"message"`
}

func DeriveStablecoinHealthWarnings(symbol string, eligibleReserveCount int, eligibilityReason string) []StablecoinHealthWarning {
	if eligibleReserveCount != 0 {
		return []StablecoinHealthWarning{}
	}
	return []StablecoinHealthWarning{{
		Code:    "no_eligible_reserve",
		Level:   "critical",
		Message: fmt.Sprintf("%s has no eligible Safe reserve: %s.", symbol, eligibilityReason),
	}}
}

func earnISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
