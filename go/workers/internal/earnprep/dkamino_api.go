package earnprep

import (
	"math/big"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// This file ports the Kamino deposit-instructions client half of
// web/packages/smart-account-vaults/src/client.ts that the Earn deposit
// family builds on: the amount formatting sent to the API (CL:2400-2416),
// the upstream error (CL:2635-2643) and, further down, the POST
// (CL:2691-2740) and the bundle decode (CL:2514-2579). The withdraw client
// and the browser response cache are not ported: the cache only ever runs in
// a browser window (CL:2659-2665), so the server path Go serves always
// fetched fresh.

// KaminoDepositInstructionsURL is KAMINO_DEPOSIT_INSTRUCTIONS_URL (CL:343-344),
// the endpoint the server side posts to; browsers use a Next proxy path
// instead (CL:347-350), which a Go server never needs.
const KaminoDepositInstructionsURL = "https://api.kamino.finance/ktx/klend/deposit-instructions"

// KaminoDepositUSDCDecimals is EARN_DEPOSIT_USDC_DECIMALS (CL:327), the
// decimals every Earn product mint has on the Kamino API wire today.
const KaminoDepositUSDCDecimals = 6

// KaminoAPIDepositDiscriminator is KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// (loyal-actions/src/constants.ts:219-221): mainnet KLend's
// deposit_reserve_liquidity_and_obligation_collateral_v2, the instruction the
// API answers with; the same bytes as kamino.DepositV2Discriminator.
var KaminoAPIDepositDiscriminator = kamino.DepositV2Discriminator

// KaminoAPIDevnetDepositDiscriminator is
// KAMINO_DEVNET_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// (constants.ts:225-227): devnet KLend's older deposit_reserve_liquidity.
var KaminoAPIDevnetDepositDiscriminator = anchor.Instruction("deposit_reserve_liquidity")

// KaminoUpstreamError ports KaminoUpstreamError (CL:2635-2643): a non-2xx
// Kamino API reply carries its status so callers can tell upstream trouble
// from a shape the API should never send.
type KaminoUpstreamError struct {
	Status  int
	Message string
}

func (e *KaminoUpstreamError) Error() string { return e.Message }

// FormatRawTokenAmountForAPI ports formatRawTokenAmountForApi (CL:2400-2416):
// the raw integer amount as whole.fraction with the fraction zero-padded to
// decimals and its trailing zeros trimmed; no fraction at all when it is zero
// ("1000000", 6) -> "1"; (1500000, 6) -> "1.5").
func FormatRawTokenAmountForAPI(amountRaw uint64, decimals int) string {
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, fraction := new(big.Int).QuoRem(new(big.Int).SetUint64(amountRaw), base, new(big.Int))
	if fraction.Sign() == 0 {
		return whole.String()
	}
	digits := []byte(fraction.String())
	for len(digits) < decimals {
		digits = append([]byte{'0'}, digits...)
	}
	return whole.String() + "." + strings.TrimRight(string(digits), "0")
}
