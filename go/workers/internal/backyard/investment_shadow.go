package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"
)

// InvestmentShadowConfig deliberately has no database or signing capability.
// The standalone command installs a read-only transport before calling this.
type InvestmentShadowConfig struct {
	RPCURL, JupiterKey, Vault, Lane string
}

type shadowSwap struct {
	Request JupiterSwapRequest `json:"request"`
	Message []byte             `json:"message"`
}

type investmentShadowCapture struct {
	Schema                  string             `json:"schema"`
	Mode                    string             `json:"mode"`
	Vault                   string             `json:"vault"`
	RouteKey                string             `json:"routeKey"`
	Lane                    string             `json:"lane"`
	CapturedAt              time.Time          `json:"capturedAt"`
	CompletedAt             time.Time          `json:"completedAt"`
	Policy                  SelectorPolicy     `json:"policy"`
	Market                  *LaneEconomics     `json:"market,omitempty"`
	CurrentQuote            *MoveQuote         `json:"currentQuote,omitempty"`
	CurrentForecast         *CandidateForecast `json:"currentForecast,omitempty"`
	CurrentEconomicEligible *bool              `json:"currentEconomicEligible,omitempty"`
	EconomicError           string             `json:"economicError,omitempty"`
	CaptureError            string             `json:"captureError,omitempty"`
	PurchaseRaw             uint64             `json:"purchaseRaw"`
	CashAccount             string             `json:"cashAccount"`
	CollateralAccount       string             `json:"collateralAccount"`
	CollateralMint          string             `json:"collateralMint"`
	CollateralDecimals      uint8              `json:"collateralDecimals"`
	ExistingPosition        KaminoPosition     `json:"existingPosition"`
	Buy                     *shadowSwap        `json:"buy,omitempty"`
	Sell                    *shadowSwap        `json:"sell,omitempty"`
	ExitPortion             *shadowSwap        `json:"exitPortion,omitempty"`
	ExitReferenceRaw        uint64             `json:"exitReferenceRaw"`
	LamportValueMicros      float64            `json:"lamportValueMicros"`
	Accounts                []ConfirmedAccount `json:"accounts,omitempty"`
	AccountSlots            []uint64           `json:"accountSlots,omitempty"`
}

// CaptureInvestmentShadow records hypothetical new-cash economics and unsigned
// conversion wires. It neither decides for a running worker nor persists targets.
func CaptureInvestmentShadow(ctx context.Context, out io.Writer, cfg InvestmentShadowConfig) error {
	v, err := findVault(cfg.Vault)
	if err != nil {
		return err
	}
	route, err := v.route(cfg.Lane)
	if err != nil {
		return err
	}
	if cfg.RPCURL == "" || cfg.JupiterKey == "" {
		return fmt.Errorf("read RPC and Jupiter credentials are required")
	}
	if v.AdminPerformanceFeeBPS != 0 {
		return fmt.Errorf("shadow comparison requires explicit performance fee modeling")
	}
	result := investmentShadowCapture{Schema: "loyal-investment-shadow/v1", Mode: "observation-only", Vault: v.VoltrVault, RouteKey: v.RouteKey, Lane: route.Lane, CapturedAt: time.Now().UTC(), Policy: v.selectorPolicy(), PurchaseRaw: 1_000_000_000, CashAccount: v.cash, CollateralAccount: route.CollateralCustody, CollateralMint: route.Kamino.CollateralMint}
	err = captureInvestmentShadow(ctx, cfg, v, route, &result)
	if err != nil {
		result.CaptureError = shadowError(err)
	}
	result.CompletedAt = time.Now().UTC()
	if result.CurrentQuote != nil && (!freshAt(result.CompletedAt, result.CurrentQuote.ObservedAt, result.Policy.QuoteMaxAge) ||
		!freshAt(result.CompletedAt, result.Market.ObservedAt, result.Policy.MarketMaxAge) ||
		!freshAt(result.CompletedAt, result.Market.NativeObservedAt, result.Policy.NativeMaxAge)) {
		result.EconomicError = "comparison_evidence_expired_during_capture"
	}
	return json.NewEncoder(out).Encode(result)
}

func shadowError(err error) string {
	var causes []string
	for err != nil {
		causes = append(causes, chain.Scrub(err.Error()))
		err = errors.Unwrap(err)
	}
	return strings.Join(causes, ": ")
}

func captureInvestmentShadow(ctx context.Context, cfg InvestmentShadowConfig, v *Vault, route RuntimeRoute, result *investmentShadowCapture) error {
	cluster, err := chain.New(cfg.RPCURL, 25*time.Second)
	if err != nil {
		return err
	}
	view, err := OpenView(ctx, cluster, nil, v.Name)
	if err != nil {
		return err
	}
	client, err := jupiter.NewClient(jupiter.KeyedBase, cfg.JupiterKey, http.DefaultClient)
	if err != nil {
		return err
	}
	feed, err := NewEconomicFeed(view)
	if err != nil {
		return err
	}
	// A fresh feed uses the same public reserve history and rate formulas as the
	// selector. Missing evidence stays unknown, rather than becoming a zero rate.
	_ = feed.Refresh(ctx)
	markets, failure := feed.Snapshot()
	for i := range markets {
		if markets[i].Lane == route.Lane {
			result.Market = &markets[i]
			break
		}
	}
	if result.Market == nil {
		result.EconomicError = "market_unavailable:" + failure
	} else if err := result.Market.validate(time.Now().UTC(), result.Policy); err != nil {
		result.EconomicError = chain.Scrub(err.Error())
	}
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, v, cluster, route, 0)
	if err != nil {
		return err
	}
	result.ExistingPosition = position
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return err
	}
	result.CollateralDecimals = prices.collateralDecimals
	// A connector-free view is frozen at its seed. Use that real baseline as
	// the minimum; the pricer's RPC simulations still refresh reserve prices.
	pricingSlot := int64(view.seedHigh)
	pricer, err := newLanePricer(ctx, cluster, view, client, v.manifest, pricingSlot)
	if err != nil {
		result.EconomicError = shadowError(err)
	}
	if err == nil {
		lamports, priceErr := pricer.lamportsMicros(1_000_000_000)
		if priceErr != nil {
			result.EconomicError = shadowError(priceErr)
		} else {
			result.LamportValueMicros = float64(lamports) / 1_000_000_000
		}
	}
	if result.Market != nil && result.EconomicError == "" {
		s := Snapshot{Fresh: true, Slot: pricingSlot, ObservationID: fmt.Sprintf("investment-shadow:%s:%d", v.RouteKey, pricingSlot), VoltrIdleRaw: int64(result.PurchaseRaw), TotalVaultNAVRaw: int64(result.PurchaseRaw)}
		q, quoteErr := pricer.priceEntry(ctx, Observation{Snapshot: s, ObservedAt: time.Now().UTC()}, *result.Market, result.PurchaseRaw)
		if quoteErr != nil {
			result.EconomicError = shadowError(quoteErr)
		} else {
			result.CurrentQuote = &q
			forecast := CandidateForecast{Lane: route.Lane}
			scoreMove(&forecast, s, result.Policy, *result.Market, q, int64(result.PurchaseRaw), 0, result.Policy.Horizon.Hours()/(365.25*24))
			result.CurrentForecast = &forecast
			eligible := forecast.BlockedReason == "" && forecast.BenefitRaw > float64(result.Policy.MinimumBenefitRaw)
			result.CurrentEconomicEligible = &eligible
		}
	}
	// Capture executable conversion wires separately from the economic forecast.
	// Lending, borrowing and full worker eligibility remain explicit separate checks.
	slot, accounts, _, err = observeSelectorDestinationBatch(ctx, v, cluster, route, 0)
	if err != nil {
		return err
	}
	prices, err = batchLanePrices(accounts, route)
	if err != nil {
		return err
	}
	quote := func(action Action, amount, floor uint64) (*shadowSwap, error) {
		if amount > math.MaxInt64 {
			return nil, fmt.Errorf("shadow amount overflow")
		}
		e, err := prepareJupiterQuoteEvidence(ctx, cluster, client, v.manifest, pricer.policies, Decision{Action: action, StrategyKey: route.Lane, AmountRaw: int64(amount), MaxSlippageBPS: 20}, amount, 0, slot)
		if err != nil {
			return nil, err
		}
		guarded, err := floorWithdrawalConversion(e.Request, ExpectedSale{}, floor)
		if err != nil {
			return nil, err
		}
		r := guarded.(JupiterSwapRequest)
		message, err := compileJupiterMessageForDelegate(v, r, mustKey(backyardDelegate))
		if err != nil {
			return nil, err
		}
		return &shadowSwap{Request: r, Message: message}, nil
	}
	buyFloor, err := valueBetweenTokenRaw(result.PurchaseRaw*9980/10000, 6, prices.collateralDecimals, prices.usdc, prices.collateral, false)
	if err != nil {
		return err
	}
	result.Buy, err = quote(SwapStableToCollateralStep, result.PurchaseRaw, buyFloor)
	if err != nil {
		return err
	}
	bought := result.Buy.Request.QuotedOutputRaw
	boughtValue, err := prices.collateralMicros(bought)
	if err != nil {
		return err
	}
	result.Sell, err = quote(SwapCollateralToStableStep, bought, uint64(boughtValue)*9980/10000)
	if err != nil {
		return err
	}
	portion, err := executionTokenLimit(prices, route, route.Kamino.CollateralMint)
	if err != nil {
		return err
	}
	reference, err := prices.collateralMicros(portion)
	if err != nil {
		return err
	}
	result.ExitReferenceRaw = uint64(reference)
	result.ExitPortion, err = quote(SwapCollateralToStableStep, portion, uint64(reference)*9980/10000)
	if err != nil {
		return err
	}
	keys := []string{budgetClockAddress, v.cash, route.CollateralCustody, route.Kamino.Obligation}
	for _, swap := range []*shadowSwap{result.Buy, result.Sell, result.ExitPortion} {
		message := swap.Message
		for i := 0; i < int(message[41]); i++ {
			keys = append(keys, solana.PublicKeyFromBytes(message[42+i*32:42+(i+1)*32]).String())
		}
	}
	result.Accounts, result.AccountSlots, err = captureShadowAccounts(ctx, cfg.RPCURL, keys, uint64(slot))
	return err
}

func captureShadowAccounts(ctx context.Context, url string, keys []string, minimumSlot uint64) ([]ConfirmedAccount, []uint64, error) {
	client := rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(url, &jsonrpc.RPCClientOpts{HTTPClient: http.DefaultClient}))
	keys = uniqueNonzero(keys)
	seen := map[string]bool{}
	var accounts []ConfirmedAccount
	for len(keys) > 0 {
		n := min(len(keys), 100)
		batch := keys[:n]
		keys = keys[n:]
		public := make([]solana.PublicKey, n)
		for i, key := range batch {
			public[i] = solana.MustPublicKeyFromBase58(key)
			seen[key] = true
		}
		got, err := client.GetMultipleAccountsWithOpts(ctx, public, &rpc.GetMultipleAccountsOpts{Encoding: solana.EncodingBase64, Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimumSlot})
		if err != nil {
			return nil, nil, err
		}
		if len(got.Value) != n {
			return nil, nil, fmt.Errorf("incomplete shadow account batch")
		}
		for i, a := range got.Value {
			if a == nil {
				accounts = append(accounts, ConfirmedAccount{Address: batch[i]})
				continue
			}
			data := a.Data.GetBinary()
			accounts = append(accounts, ConfirmedAccount{Address: batch[i], Owner: a.Owner.String(), Lamports: a.Lamports, Data: data, Executable: a.Executable})
			if a.Executable && a.Owner.String() == "BPFLoaderUpgradeab1e11111111111111111111111" && len(data) == 36 && binary.LittleEndian.Uint32(data[:4]) == 2 {
				key := solana.PublicKeyFromBytes(data[4:36]).String()
				if !seen[key] {
					keys = append(keys, key)
					seen[key] = true
				}
			}
		}
	}
	// Re-read the discovered programs and trading accounts in one bank. Mixing
	// a pool update with older vault/token accounts can invent a trading profit.
	if len(accounts) > 100 {
		return nil, nil, fmt.Errorf("shadow snapshot exceeds one atomic RPC batch")
	}
	public := make([]solana.PublicKey, len(accounts))
	for i, a := range accounts {
		public[i] = solana.MustPublicKeyFromBase58(a.Address)
	}
	got, err := client.GetMultipleAccountsWithOpts(ctx, public, &rpc.GetMultipleAccountsOpts{Encoding: solana.EncodingBase64, Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimumSlot})
	if err != nil {
		return nil, nil, err
	}
	if len(got.Value) != len(accounts) {
		return nil, nil, fmt.Errorf("incomplete atomic shadow account batch")
	}
	for i, a := range got.Value {
		address := accounts[i].Address
		accounts[i] = ConfirmedAccount{Address: address}
		if a != nil {
			accounts[i] = ConfirmedAccount{Address: address, Owner: a.Owner.String(), Lamports: a.Lamports, Data: a.Data.GetBinary(), Executable: a.Executable}
			data := accounts[i].Data
			if a.Executable && len(data) == 36 && a.Owner.String() == "BPFLoaderUpgradeab1e11111111111111111111111" && !seen[solana.PublicKeyFromBytes(data[4:]).String()] {
				return nil, nil, fmt.Errorf("program changed during shadow capture; retry")
			}
		}
	}
	return accounts, []uint64{got.Context.Slot}, nil
}
