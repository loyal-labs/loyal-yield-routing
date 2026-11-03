package webapp

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

const (
	depositFeeInput   = "100"
	depositFeeTTL     = 10 * time.Minute
	depositFeeRetry   = time.Minute
	depositFeeTimeout = 5 * time.Second
)

type depositCell struct {
	chain bridge.Chain
	asset bridge.Asset
}

type depositFee struct {
	percent    *string
	until      time.Time
	refreshing bool
}

type depositFees struct {
	mu   sync.Mutex
	held map[depositCell]depositFee
}

var privyDepositFeeCache = depositFees{held: map[depositCell]depositFee{}}

// privyDepositFees returns what the cache holds for every cell of the
// networks, nil where it holds nothing, and never waits on Privy. A cell with
// no fresh answer and no refresh running starts one in the background.
func (h earnRoutes) privyDepositFees(networks []bridge.Network) map[depositCell]*string {
	now := time.Now()
	out := map[depositCell]*string{}
	privyDepositFeeCache.mu.Lock()
	defer privyDepositFeeCache.mu.Unlock()
	for _, network := range networks {
		for _, token := range network.Tokens {
			cell := depositCell{network.Chain, token.Asset}
			fee := privyDepositFeeCache.held[cell]
			out[cell] = fee.percent
			if now.Before(fee.until) || fee.refreshing {
				continue
			}
			fee.refreshing = true
			privyDepositFeeCache.held[cell] = fee
			go h.refreshDepositFee(cell, token.Address)
		}
	}
	return out
}

// refreshDepositFee quotes one cell. It keeps the last answer when the quote
// fails and tries again after depositFeeRetry.
func (h earnRoutes) refreshDepositFee(cell depositCell, address string) {
	ctx, cancel := context.WithTimeout(context.Background(), depositFeeTimeout)
	defer cancel()
	percent, err := h.privyDepositFee(ctx, PrivyDepositAsset{Asset: address, Chain: string(cell.chain)})
	cache := &privyDepositFeeCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	fee := cache.held[cell]
	if err != nil {
		slog.Warn("privy deposit quote failed", "chain", cell.chain, "asset", cell.asset, "err", err)
		fee = depositFee{percent: fee.percent, until: time.Now().Add(depositFeeRetry)}
	} else {
		fee = depositFee{percent: &percent, until: time.Now().Add(depositFeeTTL)}
	}
	cache.held[cell] = fee
}

func (h earnRoutes) privyDepositFee(ctx context.Context, source PrivyDepositAsset) (string, error) {
	quote, err := h.privyQuoteDeposit(ctx, source, depositFeeInput)
	if err != nil {
		return "", err
	}
	in, inOK := new(big.Rat).SetString(quote.InputAmount)
	out, outOK := new(big.Rat).SetString(quote.EstimatedOutputAmount)
	if !inOK || !outOK || in.Sign() <= 0 || out.Sign() < 0 {
		return "", fmt.Errorf("privy deposit quote: input %q, output %q", quote.InputAmount, quote.EstimatedOutputAmount)
	}
	cost := new(big.Rat).Sub(in, out)
	return cost.Mul(cost, big.NewRat(100, 1)).Quo(cost, in).FloatString(3), nil
}
