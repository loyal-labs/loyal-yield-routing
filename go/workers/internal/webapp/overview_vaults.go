package webapp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"golang.org/x/sync/errgroup"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	overviewVaultsPerCall  = 100
	overviewSOLMint        = "So11111111111111111111111111111111111111112"
	overviewSOLDecimals    = 9
	overviewDevnetUSDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	overviewUSDCIcon       = "https://coin-images.coingecko.com/coins/images/6319/large/usdc.png"
	overviewAssetBodyLimit = 1 << 20
)

type overviewVaultsInput struct {
	SessionInput
	Invalidate         string `query:"invalidate"`
	AccountUtilization string `query:"accountUtilization"`
}

type overviewAsset struct {
	Mint     string  `json:"mint"`
	Symbol   string  `json:"symbol"`
	Name     string  `json:"name"`
	Decimals float64 `json:"decimals"`
	ImageURL *string `json:"imageUrl"`
	IsNative bool    `json:"isNative"`
}

type overviewPosition struct {
	Asset          overviewAsset `json:"asset"`
	PublicBalance  float64       `json:"publicBalance"`
	TotalBalance   float64       `json:"totalBalance"`
	PriceUsd       *float64      `json:"priceUsd"`
	PublicValueUsd *float64      `json:"publicValueUsd"`
	TotalValueUsd  *float64      `json:"totalValueUsd"`
}

type overviewPortfolioTotals struct {
	TotalUsd             float64  `json:"totalUsd"`
	TotalSol             *float64 `json:"totalSol"`
	PricedCount          int      `json:"pricedCount"`
	UnpricedCount        int      `json:"unpricedCount"`
	EffectiveSolPriceUsd *float64 `json:"effectiveSolPriceUsd"`
}

type overviewPortfolio struct {
	Owner                 string                  `json:"owner"`
	NativeBalanceLamports uint64                  `json:"nativeBalanceLamports"`
	Positions             []overviewPosition      `json:"positions"`
	Totals                overviewPortfolioTotals `json:"totals"`
	FetchedAt             int64                   `json:"fetchedAt"`
}

type overviewVault struct {
	AccountIndex   int                     `json:"accountIndex"`
	Address        string                  `json:"address"`
	Lamports       uint64                  `json:"lamports"`
	Portfolio      overviewPortfolio       `json:"portfolio"`
	Activity       overviewActivityPage    `json:"activity"`
	Signers        []overviewSigner        `json:"signers"`
	SpendingLimits []overviewSpendingLimit `json:"spendingLimits"`
}

type overviewHolding struct {
	mint solana.PublicKey
	raw  *big.Int
}

type overviewTokenMeta struct {
	asset overviewAsset
	price *float64
}

type overviewAssets struct {
	gecko   *tokenDetails
	http    *http.Client
	rpcURL  string
	jupiter string
	mainnet bool
}

func newOverviewAssets(h earnRoutes) *overviewAssets {
	assets := &overviewAssets{http: h.HTTP, jupiter: jupiterSearchURL, mainnet: h.Cluster == Mainnet}
	if key, _ := engine.OptionalCredential("COINGECKO_API_KEY"); key != "" {
		assets.gecko = newTokenDetails(key)
	}
	assets.rpcURL, _ = engine.OptionalCredential("WEB_SOLANA_RPC_URL")
	return assets
}

func overviewInvalidates(raw string) (bool, error) {
	found := false
	for part := range strings.SplitSeq(raw, ",") {
		address := jsTrim(part)
		if address == "" {
			continue
		}
		if _, err := solana.PublicKeyFromBase58(address); err != nil {
			return false, fmt.Errorf("invalidate: invalid public key input")
		}
		found = true
	}
	return found, nil
}

func overviewParseInt(raw string) (float64, bool) {
	s := jsTrim(raw)
	sign := 1.0
	if s != "" && (s[0] == '+' || s[0] == '-') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	value, _ := strconv.ParseFloat(s[:end], 64)
	return sign * value, true
}

func (o *overviewRoutes) vaults(ctx context.Context, settingsPda solana.PublicKey, highest float64, given bool) ([]overviewVault, error) {
	if !given {
		settings, err := o.settingsAccount(ctx, settingsPda)
		if err != nil {
			return nil, err
		}
		highest = float64(settings.AccountUtilization)
	}
	count := math.Max(highest+1, 1)
	if count > overviewVaultsPerCall {
		return nil, fmt.Errorf("%v vault accounts exceed one getMultipleAccounts call", count)
	}
	addresses := make([]solana.PublicKey, int(count))
	for index := range addresses {
		vault, _, err := squads.SmartAccountAddress(settingsPda, uint8(index))
		if err != nil {
			return nil, err
		}
		addresses[index] = vault
	}
	_, accounts, err := o.Chain.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, fmt.Errorf("read vault balances: %w", err)
	}
	holdings := make([][]overviewHolding, len(addresses))
	reads, readCtx := errgroup.WithContext(ctx)
	for i, vault := range addresses {
		reads.Go(func() (err error) {
			holdings[i], err = o.vaultHoldings(readCtx, vault)
			return err
		})
	}
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	decimals, err := o.mintDecimals(ctx, holdings)
	if err != nil {
		return nil, err
	}
	metas := o.assets.resolve(ctx, decimals)
	fetchedAt := time.Now().UnixMilli()
	out := make([]overviewVault, len(addresses))
	for i, vault := range addresses {
		var lamports uint64
		if accounts[i] != nil {
			lamports = accounts[i].Lamports
		}
		out[i] = overviewVault{
			AccountIndex:   i,
			Address:        vault.String(),
			Lamports:       lamports,
			Portfolio:      o.assets.portfolio(vault.String(), lamports, holdings[i], metas, fetchedAt),
			Activity:       overviewActivityPage{Activities: []any{}},
			Signers:        []overviewSigner{},
			SpendingLimits: []overviewSpendingLimit{},
		}
	}
	return out, nil
}

func (o *overviewRoutes) vaultHoldings(ctx context.Context, vault solana.PublicKey) ([]overviewHolding, error) {
	var holdings []overviewHolding
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		_, accounts, err := o.Chain.TokenAccounts(ctx, vault, program, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return nil, fmt.Errorf("read vault token accounts: %w", err)
		}
		for i := range accounts {
			token, err := spl.DecodeTokenAccount(&accounts[i])
			if err != nil || token.Amount == 0 {
				continue
			}
			mint := token.Mint
			amount := new(big.Int).SetUint64(token.Amount)
			if at := slices.IndexFunc(holdings, func(h overviewHolding) bool { return h.mint == mint }); at >= 0 {
				holdings[at].raw.Add(holdings[at].raw, amount)
				continue
			}
			holdings = append(holdings, overviewHolding{mint: mint, raw: amount})
		}
	}
	return holdings, nil
}

func (o *overviewRoutes) mintDecimals(ctx context.Context, holdings [][]overviewHolding) (map[string]float64, error) {
	decimals := map[string]float64{overviewSOLMint: overviewSOLDecimals}
	var mints []solana.PublicKey
	for _, vault := range holdings {
		for _, holding := range vault {
			if !slices.Contains(mints, holding.mint) {
				mints = append(mints, holding.mint)
			}
		}
	}
	if len(mints) == 0 {
		return decimals, nil
	}
	_, accounts, err := o.Chain.Accounts(ctx, mints, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, fmt.Errorf("read vault token mints: %w", err)
	}
	for i, account := range accounts {
		if account == nil {
			continue
		}
		if mint, err := spl.DecodeMint(account); err == nil {
			decimals[mints[i].String()] = float64(mint.Decimals)
		}
	}
	return decimals, nil
}

func (a *overviewAssets) resolve(ctx context.Context, decimals map[string]float64) map[string]overviewTokenMeta {
	metas := make(map[string]overviewTokenMeta, len(decimals))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for mint, places := range decimals {
		wg.Go(func() {
			meta := a.meta(ctx, mint, places)
			mu.Lock()
			metas[mint] = meta
			mu.Unlock()
		})
	}
	wg.Wait()
	return metas
}

func (a *overviewAssets) meta(ctx context.Context, mint string, decimals float64) overviewTokenMeta {
	if mint == overviewSOLMint {
		var price *float64
		if market := a.market(ctx, mint); market != nil {
			price = overviewPositive(market.priceUSD)
		}
		return overviewTokenMeta{asset: overviewAsset{Mint: mint, Symbol: "SOL", Name: "Solana", Decimals: overviewSOLDecimals, IsNative: true}, price: price}
	}
	if mint == overviewDevnetUSDCMint {
		icon, one := overviewUSDCIcon, 1.0
		return overviewTokenMeta{asset: overviewAsset{Mint: mint, Symbol: "USDC", Name: "USD Coin", Decimals: decimals, ImageURL: &icon}, price: &one}
	}
	market := a.market(ctx, mint)
	var symbol, name, image *string
	var price *float64
	if market != nil {
		symbol, name, image = overviewTrimmed(market.symbol), overviewTrimmed(market.name), market.imageURL
		price = overviewPositive(market.priceUSD)
		if market.decimals != nil {
			decimals = *market.decimals
		}
	}
	if price == nil && a.mainnet {
		price = a.jupiterPrice(ctx, mint)
	}
	if symbol == nil {
		if onchain := a.onchain(ctx, mint); onchain != nil {
			symbol = onchain.symbol
			name = cmp.Or(name, onchain.name)
			if image == nil {
				image = onchain.image
			}
		}
	}
	asset := overviewAsset{Mint: mint, Symbol: "TOKEN", Name: "Token", Decimals: decimals, ImageURL: image}
	if symbol != nil {
		asset.Symbol, asset.Name = *symbol, *symbol
	}
	if name != nil {
		asset.Name = *name
	}
	return overviewTokenMeta{asset: asset, price: price}
}

func (a *overviewAssets) market(ctx context.Context, mint string) *geckoToken {
	if a.gecko == nil {
		return nil
	}
	return a.gecko.token(ctx, mint)
}

func overviewTrimmed(value *string) *string {
	if value == nil {
		return nil
	}
	if trimmed := jsTrim(*value); trimmed != "" {
		return &trimmed
	}
	return nil
}

func overviewPositive(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 {
		return nil
	}
	return value
}

func (a *overviewAssets) jupiterPrice(ctx context.Context, mint string) *float64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jupiter+"?"+url.Values{"query": {mint}}.Encode(), nil)
	if err != nil {
		return nil
	}
	var tokens []struct {
		ID       any `json:"id"`
		UsdPrice any `json:"usdPrice"`
	}
	if overviewFetchJSON(a.http, req, &tokens) != nil {
		return nil
	}
	for _, token := range tokens {
		if token.ID == mint {
			if price, ok := token.UsdPrice.(float64); ok {
				return overviewPositive(&price)
			}
			return nil
		}
	}
	return nil
}

type overviewOnchainMeta struct{ symbol, name, image *string }

func (a *overviewAssets) onchain(ctx context.Context, mint string) *overviewOnchainMeta {
	if a.rpcURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"id": "getAsset:" + mint, "jsonrpc": "2.0", "method": "getAsset", "params": map[string]string{"id": mint}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	var resp struct {
		Result *struct {
			Content struct {
				Files []struct {
					CdnURI *string `json:"cdn_uri"`
					URI    *string `json:"uri"`
				} `json:"files"`
				Links struct {
					Image *string `json:"image"`
				} `json:"links"`
				Metadata struct {
					Name   *string `json:"name"`
					Symbol *string `json:"symbol"`
				} `json:"metadata"`
			} `json:"content"`
			TokenInfo struct {
				Symbol *string `json:"symbol"`
			} `json:"token_info"`
		} `json:"result"`
	}
	if overviewFetchJSON(a.http, req, &resp) != nil || resp.Result == nil {
		return nil
	}
	r := resp.Result
	meta := &overviewOnchainMeta{
		symbol: cmp.Or(overviewTrimmed(r.Content.Metadata.Symbol), overviewTrimmed(r.TokenInfo.Symbol)),
		name:   overviewTrimmed(r.Content.Metadata.Name),
		image:  overviewTrimmed(r.Content.Links.Image),
	}
	if meta.image == nil && len(r.Content.Files) > 0 {
		meta.image = cmp.Or(overviewTrimmed(r.Content.Files[0].CdnURI), overviewTrimmed(r.Content.Files[0].URI))
	}
	if meta.symbol == nil && meta.name == nil {
		return nil
	}
	return meta
}

func overviewFetchJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, overviewAssetBodyLimit)).Decode(out)
}

func (a *overviewAssets) portfolio(owner string, lamports uint64, holdings []overviewHolding, metas map[string]overviewTokenMeta, fetchedAt int64) overviewPortfolio {
	sol := metas[overviewSOLMint]
	type balance struct {
		meta    overviewTokenMeta
		balance float64
	}
	balances := []balance{{sol, float64(lamports) / math.Pow(10, overviewSOLDecimals)}}
	for _, holding := range holdings {
		meta, ok := metas[holding.mint.String()]
		if !ok {
			continue
		}
		if a.mainnet && meta.price == nil {
			continue
		}
		raw, _ := new(big.Float).SetInt(holding.raw).Float64()
		balances = append(balances, balance{meta, raw / math.Pow(10, meta.asset.Decimals)})
	}
	positions := make([]overviewPosition, 0, len(balances))
	for _, b := range balances {
		var value *float64
		switch {
		case b.balance == 0:
			value = new(float64)
		case b.meta.price != nil:
			v := b.balance * *b.meta.price
			value = &v
		}
		price := b.meta.price
		if price == nil && b.balance > 0 && value != nil {
			p := *value / b.balance
			price = &p
		}
		positions = append(positions, overviewPosition{Asset: b.meta.asset, PublicBalance: b.balance, TotalBalance: b.balance, PriceUsd: price, PublicValueUsd: value, TotalValueUsd: value})
	}
	slices.SortStableFunc(positions, func(left, right overviewPosition) int {
		valueOf := func(p overviewPosition) float64 {
			if p.TotalValueUsd == nil {
				return -1
			}
			return *p.TotalValueUsd
		}
		return cmp.Or(cmp.Compare(valueOf(right), valueOf(left)), overviewLocaleCompare(left.Asset.Symbol, right.Asset.Symbol), overviewLocaleCompare(left.Asset.Mint, right.Asset.Mint))
	})
	return overviewPortfolio{Owner: owner, NativeBalanceLamports: lamports, Positions: positions, Totals: overviewTotals(positions), FetchedAt: fetchedAt}
}

func overviewTotals(positions []overviewPosition) overviewPortfolioTotals {
	var totals overviewPortfolioTotals
	var sum float64
	for _, p := range positions {
		switch {
		case p.TotalValueUsd != nil:
			sum += *p.TotalValueUsd
			totals.PricedCount++
		case p.TotalBalance > 0:
			totals.UnpricedCount++
		}
	}
	if at := slices.IndexFunc(positions, func(p overviewPosition) bool { return p.Asset.Mint == overviewSOLMint }); at >= 0 {
		totals.EffectiveSolPriceUsd = positions[at].PriceUsd
	}
	totals.TotalUsd = overviewFloor(sum, 2)
	if price := totals.EffectiveSolPriceUsd; price != nil && *price > 0 {
		sol := overviewFloor(totals.TotalUsd / *price, 4)
		totals.TotalSol = &sol
	}
	return totals
}

func overviewFloor(value float64, places int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	factor := math.Pow(10, float64(places))
	return math.Floor(value*factor) / factor
}
