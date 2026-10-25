package webapp

import (
	"fmt"
	"strconv"
	"time"
)

// Serializes Earn history into the exact JSON the Next
// web/apps/web/src/app/api/smart-accounts/earn-transactions/formatter.ts
// emits: same field names, same key order, same null/omit behaviour.

const (
	txreadsAutodepositLabel = "Autodeposit"
	txreadsMainUSDCLabel    = "Main"
	txreadsEarnVaultLabel   = "Earn"
	txreadsMainUSDCIcon     = "/wallet-workspace/earn-vault-usdc.png"
)

var txreadsCompactMarketNames = map[string]string{
	"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF": "Main Kamino",
	"27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J": "Main Kamino",
	"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA": "Prime",
	"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y": "Maple",
	"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8": "OnRe",
	"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF": "Ethena",
	"DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek": "JLP",
	"GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs": "Bitcoin",
	"CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw": "Superstate",
	"52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf": "Huma",
	"9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU": "Solstice",
}

var txreadsMarketIcons = map[string]string{
	"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF": "/wallet-workspace/earn-kamino.png",
	"27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J": "/wallet-workspace/earn-kamino.png",
	"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA": "/wallet-workspace/earn-prime.png",
	"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y": "/wallet-workspace/earn-maple.svg",
	"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8": "/wallet-workspace/earn-onre.png",
	"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF": "/wallet-workspace/earn-ethena.png",
}

var txreadsMintSymbols = map[string]string{
	"CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH": "CASH",
	"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH": "USDG",
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD",
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
	"USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA":  "USDS",
	"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU": "USDC",
}

var txreadsMonths = [...]string{"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December"}

func txreadsShortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:4] + "..." + address[len(address)-4:]
}

func txreadsMarketLabel(liquidityMint, market, reserve *string) string {
	switch {
	case txreadsPresent(market):
		if name, ok := txreadsCompactMarketNames[*market]; ok {
			return name
		}
		return txreadsShortenAddress(*market)
	case txreadsPresent(reserve):
		return txreadsShortenAddress(*reserve)
	case txreadsPresent(liquidityMint):
		if symbol, ok := txreadsMintSymbols[*liquidityMint]; ok {
			return symbol
		}
		return txreadsShortenAddress(*liquidityMint)
	default:
		return "position"
	}
}

func txreadsMarketIcon(market *string) string {
	if txreadsPresent(market) {
		if icon, ok := txreadsMarketIcons[*market]; ok {
			return icon
		}
		return "/wallet-workspace/earn-kamino.png"
	}
	return "/wallet-workspace/earn-kamino.png"
}

func txreadsPresent(value *string) bool {
	return value != nil && *value != ""
}

func txreadsFormatExactUSDC(amount int64) string {
	sign := ""
	absolute := uint64(amount)
	if amount < 0 {
		sign = "-"
		absolute = uint64(-amount)
	}
	return sign + "$" + strconv.FormatUint(absolute/1_000_000, 10) + "." +
		fmt.Sprintf("%06d", absolute%1_000_000)
}

// txreadsFormatDisplayUSDC mirrors formatDisplayUsdcAmount: the sign comes
// from the direction alone, cents round half away from zero in whole cents.
func txreadsFormatDisplayUSDC(amount int64, direction string) string {
	sign := ""
	switch direction {
	case "in":
		sign = "+"
	case "out":
		sign = "-"
	}
	absolute := uint64(amount)
	if amount < 0 {
		absolute = uint64(-amount)
	}
	cents := uint64(0)
	if absolute != 0 {
		cents = (absolute + 9_999) / 10_000
	}
	return sign + "$" + strconv.FormatUint(cents/100, 10) + "." +
		fmt.Sprintf("%02d", cents%100)
}

func txreadsDateGroup(at time.Time) string {
	utc := at.UTC()
	return txreadsMonths[utc.Month()-1] + " " + strconv.Itoa(utc.Day())
}

func txreadsTimestamp(at time.Time) string {
	utc := at.UTC()
	hour := utc.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	meridiem := "AM"
	if utc.Hour() >= 12 {
		meridiem = "PM"
	}
	return strconv.Itoa(hour) + ":" + fmt.Sprintf("%02d", utc.Minute()) + " " + meridiem
}
