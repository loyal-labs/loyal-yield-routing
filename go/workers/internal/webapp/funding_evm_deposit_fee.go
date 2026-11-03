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
	percent *string
	until   time.Time
}

type depositFees struct {
	mu   sync.Mutex
	held map[depositCell]depositFee
}

var privyDepositFeeCache = depositFees{held: map[depositCell]depositFee{}}

// privyDepositFees returns Privy's auto-bridge cost for every cell of the
// networks, quoting the cells it holds no fresh answer for. A failed quote is
// nil and is retried after depositFeeRetry.
func (h earnRoutes) privyDepositFees(ctx context.Context, networks []bridge.Network) map[depositCell]*string {
	now := time.Now()
	out := map[depositCell]*string{}
	type stale struct {
		cell  depositCell
		token bridge.Token
	}
	var quote []stale
	privyDepositFeeCache.mu.Lock()
	for _, network := range networks {
		for _, token := range network.Tokens {
			cell := depositCell{network.Chain, token.Asset}
			if fee, ok := privyDepositFeeCache.held[cell]; ok && now.Before(fee.until) {
				out[cell] = fee.percent
			} else {
				quote = append(quote, stale{cell, token})
			}
		}
	}
	privyDepositFeeCache.mu.Unlock()
	if len(quote) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, depositFeeTimeout)
	defer cancel()
	fresh := make([]depositFee, len(quote))
	var wg sync.WaitGroup
	for i, s := range quote {
		wg.Add(1)
		go func() {
			defer wg.Done()
			percent, err := h.privyDepositFee(ctx, PrivyDepositAsset{Asset: s.token.Address, Chain: string(s.cell.chain)})
			if err != nil {
				slog.Warn("privy deposit quote failed", "chain", s.cell.chain, "asset", s.cell.asset, "err", err)
				fresh[i] = depositFee{until: time.Now().Add(depositFeeRetry)}
				return
			}
			fresh[i] = depositFee{percent: &percent, until: time.Now().Add(depositFeeTTL)}
		}()
	}
	wg.Wait()
	privyDepositFeeCache.mu.Lock()
	defer privyDepositFeeCache.mu.Unlock()
	for i, s := range quote {
		privyDepositFeeCache.held[s.cell] = fresh[i]
		out[s.cell] = fresh[i].percent
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
