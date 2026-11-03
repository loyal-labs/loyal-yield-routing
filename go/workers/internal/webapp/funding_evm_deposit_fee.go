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
	value      *string
	until      time.Time
	refreshing bool
}

type depositFees struct {
	mu      sync.Mutex
	held    map[depositCell]depositFee
	timeout time.Duration
}

var privyDepositFeeCache = depositFees{held: map[depositCell]depositFee{}, timeout: depositFeeTimeout}

func (c *depositFees) read(cell depositCell, quote func(context.Context) (string, error)) *string {
	c.mu.Lock()
	defer c.mu.Unlock()
	fee := c.held[cell]
	if time.Now().Before(fee.until) || fee.refreshing {
		return fee.value
	}
	fee.refreshing = true
	c.held[cell] = fee
	go c.refresh(cell, quote)
	return fee.value
}

func (c *depositFees) refresh(cell depositCell, quote func(context.Context) (string, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	value, err := quote(ctx)
	if err != nil {
		slog.Warn("deposit fee quote failed", "chain", cell.chain, "asset", cell.asset, "err", err)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.held[cell] = depositFee{value: c.held[cell].value, until: time.Now().Add(depositFeeRetry)}
		return
	}
	c.store(cell, value)
}

func (c *depositFees) store(cell depositCell, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[cell] = depositFee{value: &value, until: time.Now().Add(depositFeeTTL)}
}

func (h earnRoutes) privyDepositFees(networks []bridge.Network) map[depositCell]*string {
	out := map[depositCell]*string{}
	for _, network := range networks {
		for _, token := range network.Tokens {
			source := PrivyDepositAsset{Asset: token.Address, Chain: string(network.Chain)}
			cell := depositCell{network.Chain, token.Asset}
			out[cell] = privyDepositFeeCache.read(cell, func(ctx context.Context) (string, error) { return h.privyDepositFee(ctx, source) })
		}
	}
	return out
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
