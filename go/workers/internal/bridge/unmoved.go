package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"golang.org/x/sync/errgroup"
)

const (
	unmovedInterval = 10 * time.Minute
	unmovedDeadline = 3 * time.Minute

	relaySignedAddresses = `SELECT evm_address, array_agg(solana_wallet) FROM loyal_yield.evm_deposit_routes
WHERE kind = 'relay_signed' GROUP BY evm_address ORDER BY evm_address`

	recentTransferWallets = `SELECT DISTINCT solana_wallet FROM loyal_yield.bridge_transfers
WHERE direction = 'in' AND confirmed_at > now() - interval '30 minutes'`
)

// PrivyGet is one GET against Privy's API; path starts at /v1.
type PrivyGet func(ctx context.Context, path string, query url.Values, out any) error

type unmovedRoute struct {
	address string
	wallets []string
}

type unmovedRead struct {
	walletID string
	balances []Balance
	err      error
}

func (t *Tracker) unmovedEvery(ctx context.Context) {
	walletIDs := map[string]string{}
	ticker := time.NewTicker(unmovedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pass, cancel := context.WithTimeout(ctx, unmovedDeadline)
		err := t.unmoved(pass, walletIDs)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("bridge incoming scan failed", "error", engine.ErrorText(err))
		}
	}
}

// unmoved leaves the series of an address whose read failed as they were. The
// alert rules keep the 30-minute clock.
func (t *Tracker) unmoved(ctx context.Context, walletIDs map[string]string) error {
	rows, err := t.DB.Query(ctx, relaySignedAddresses)
	if err != nil {
		return err
	}
	routes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (unmovedRoute, error) {
		var r unmovedRoute
		err := row.Scan(&r.address, &r.wallets)
		return r, err
	})
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(routes))
	for _, r := range routes {
		live[r.address] = true
	}
	for address := range walletIDs {
		if !live[address] {
			t.Facts.BridgeIncomingHeld(strings.ToLower(address), nil)
			delete(walletIDs, address)
		}
	}
	var recent []string
	if len(routes) > 0 {
		rows, err = t.DB.Query(ctx, recentTransferWallets)
		if err != nil {
			return err
		}
		recent, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
	}

	reads := make([]unmovedRead, len(routes))
	var group errgroup.Group
	group.SetLimit(trackParallel)
	for i, r := range routes {
		group.Go(func() error {
			reads[i] = t.privyBalances(ctx, walletIDs[r.address], r.address)
			return nil
		})
	}
	_ = group.Wait()

	failed := 0
	var firstErr error
	for i, r := range routes {
		read := reads[i]
		if read.walletID != "" {
			walletIDs[r.address] = read.walletID
		}
		if read.err != nil {
			failed++
			if firstErr == nil {
				firstErr = read.err
			}
			continue
		}
		var held []engine.IncomingHeld
		if !slices.ContainsFunc(r.wallets, func(wallet string) bool { return slices.Contains(recent, wallet) }) {
			for _, b := range read.balances {
				if b.AmountRaw.Cmp(tenPow(max(int64(b.Decimals)-1, 0))) < 0 {
					continue
				}
				held = append(held, engine.IncomingHeld{Chain: string(b.Chain), Asset: string(b.Asset), UnderMin: underMin(b), Units: units(b)})
				slog.Warn("bridge incoming unmoved", "address", maskAddress(r.address), "chain", b.Chain, "asset", b.Asset,
					"amount", decimalAmount(b), "under_min", underMin(b))
			}
		}
		t.Facts.BridgeIncomingHeld(strings.ToLower(r.address), held)
	}
	if failed > 0 {
		slog.Warn("bridge incoming scan incomplete", "failed_addresses", failed, "addresses", len(routes), "error", engine.ErrorText(firstErr))
	}
	t.Facts.BridgeIncomingScanned(failed)
	return nil
}

func (t *Tracker) privyBalances(ctx context.Context, walletID, address string) unmovedRead {
	if walletID == "" {
		var page struct {
			Data []struct {
				ID        string `json:"id"`
				Address   string `json:"address"`
				ChainType string `json:"chain_type"`
			} `json:"data"`
		}
		query := url.Values{"address": {address}, "chain_type": {"ethereum"}}
		if err := t.Privy(ctx, "/v1/wallets", query, &page); err != nil {
			return unmovedRead{err: err}
		}
		for _, wallet := range page.Data {
			if wallet.ChainType == "ethereum" && wallet.ID != "" && strings.EqualFold(wallet.Address, address) {
				walletID = wallet.ID
				break
			}
		}
		if walletID == "" {
			return unmovedRead{err: fmt.Errorf("privy has no ethereum wallet %s", maskAddress(address))}
		}
	}
	queries := PrivyBalanceQueries()
	pages := make([][]byte, len(queries))
	for i, query := range queries {
		var raw json.RawMessage
		if err := t.Privy(ctx, "/v1/wallets/"+url.PathEscape(walletID)+"/balance", query, &raw); err != nil {
			return unmovedRead{walletID: walletID, err: err}
		}
		pages[i] = raw
	}
	balances, err := ParsePrivyBalances(pages)
	return unmovedRead{walletID: walletID, balances: balances, err: err}
}

func maskAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:6] + "…" + address[len(address)-4:]
}

func decimalAmount(b Balance) string {
	amount := new(big.Rat).SetFrac(b.AmountRaw, tenPow(int64(b.Decimals))).FloatString(int(b.Decimals))
	if !strings.Contains(amount, ".") {
		return amount
	}
	return strings.TrimSuffix(strings.TrimRight(amount, "0"), ".")
}

func units(b Balance) float64 {
	v, _ := new(big.Rat).SetFrac(b.AmountRaw, tenPow(int64(b.Decimals))).Float64()
	return v
}

func underMin(b Balance) bool {
	n, ok := Lookup(b.Chain)
	return ok && n.MinUSD > 0 && b.AmountRaw.Cmp(new(big.Int).Mul(big.NewInt(int64(n.MinUSD)), tenPow(int64(b.Decimals)))) < 0
}
