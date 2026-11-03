package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

var earnTronDepositFamily = earnFamily{
	name:      "tron deposit address",
	appGrants: []Grant{TableGrant("app_users", "SELECT")},
	yieldGrants: []Grant{
		TableGrant("loyal_yield.relay_deposit_addresses", "SELECT"),
		TableGrant("loyal_yield.relay_deposit_addresses", "INSERT"),
	},
	ready:    func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil },
	register: earnRoutes.registerTronDeposit,
}

const tronDepositPath = "/api/funding/tron-deposit-address"

type TronDepositAddressResponse struct {
	Address     string    `json:"address"`
	MinUSD      int       `json:"minUsd"`
	FeeUSD      *string   `json:"feeUsd" doc:"Relay's fee in USD for a minimum deposit, from a deposit-address quote cached up to ten minutes; null until one has answered"`
	ReceiveUSDC *string   `json:"receiveUsdc" doc:"USDC on Solana a minimum deposit pays after Relay's fee and price, from the same quote; null until one has answered"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type tronDepositOutput struct{ Body TronDepositAddressResponse }

type tronDepositRow struct {
	Address   string
	ExpiresAt time.Time
}

var tronDepositCreates = walletLocks{held: map[solana.PublicKey]*walletLock{}}

type tronDepositPrice struct {
	FeeUSD      *string
	ReceiveUSDC *string
}

func shownText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func priceOf(quote bridge.DepositAddress) tronDepositPrice {
	return tronDepositPrice{FeeUSD: shownText(quote.FeeUSD), ReceiveUSDC: shownText(quote.ReceiveUSDC)}
}

var (
	tronDepositCell       = depositCell{bridge.Tron.Chain, bridge.Tron.Asset}
	tronDepositPriceCache = depositFees[tronDepositPrice]{held: map[depositCell]depositFee[tronDepositPrice]{}, timeout: 15 * time.Second}
)

func (h earnRoutes) tronDepositPrice(wallet solana.PublicKey) tronDepositPrice {
	price := tronDepositPriceCache.read(tronDepositCell, func(ctx context.Context) (tronDepositPrice, error) {
		quote, err := h.Relay.DepositAddress(ctx, bridge.Tron, wallet.String())
		if err == nil && quote.FeeUSD == "" {
			err = errors.New("relay deposit quote carries no relayer fee")
		}
		return priceOf(quote), err
	})
	if price == nil {
		return tronDepositPrice{}
	}
	return *price
}

func (h earnRoutes) registerTronDeposit(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-tron-deposit-address",
		Method:      http.MethodPost,
		Path:        tronDepositPath,
		Summary:     "The signed-in wallet's Tron USDT deposit address, minted on first use",
		Responses:   errorResponses[routeError](api, 401, 403, 502, 503),
	}, h.tronDepositAddress)
}

func tronDepositUnavailable(step string, err error) error {
	return earnUnavailable(step, err, "tron_deposit_unavailable", "Deposits from Tron are unavailable. Please try again.")
}

func (h earnRoutes) tronDepositAddress(ctx context.Context, in *sessionRequest) (*tronDepositOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := h.auth.Session(in.SessionInput)
	if refusal, ok := authRefusal(err); ok {
		return nil, refusal
	}
	if err != nil {
		return nil, tronDepositUnavailable("tron deposit session", err)
	}
	wallet := session.Wallet
	userID, err := h.appUserID(ctx, wallet)
	if refusal, ok := authRefusal(err); ok {
		return nil, refusal
	}
	if err != nil {
		return nil, tronDepositUnavailable("tron deposit app user", err)
	}
	row, err := h.ensureTronDepositAddress(ctx, wallet, userID)
	if err != nil {
		return nil, err
	}
	price := h.tronDepositPrice(wallet)
	return &tronDepositOutput{Body: TronDepositAddressResponse{
		Address:     row.Address,
		MinUSD:      bridge.Tron.MinUSD,
		FeeUSD:      price.FeeUSD,
		ReceiveUSDC: price.ReceiveUSDC,
		ExpiresAt:   row.ExpiresAt.UTC(),
	}}, nil
}

func (h earnRoutes) ensureTronDepositAddress(ctx context.Context, wallet solana.PublicKey, userID string) (*tronDepositRow, error) {
	unlock, err := tronDepositCreates.lock(ctx, wallet)
	if err != nil {
		return nil, tronDepositUnavailable("tron deposit lock", err)
	}
	defer unlock()
	row, err := h.storedTronDepositAddress(ctx, wallet)
	if err != nil {
		return nil, tronDepositUnavailable("tron deposit read", err)
	}
	if row != nil {
		return row, nil
	}
	minted, err := h.mintTronDepositAddress(ctx, wallet, userID)
	if err != nil {
		return nil, err
	}
	if minted.FeeUSD != "" {
		tronDepositPriceCache.store(tronDepositCell, priceOf(minted))
	}
	return &tronDepositRow{Address: minted.Address, ExpiresAt: minted.ExpiresAt}, nil
}

func (h earnRoutes) storedTronDepositAddress(ctx context.Context, wallet solana.PublicKey) (*tronDepositRow, error) {
	var row tronDepositRow
	err := h.YieldDB.QueryRow(ctx, `SELECT address, expires_at FROM loyal_yield.relay_deposit_addresses
WHERE solana_wallet = $1 AND chain = $2 AND asset = $3 AND expires_at > $4
ORDER BY created_at DESC LIMIT 1`, wallet.String(), string(bridge.Tron.Chain), string(bridge.Tron.Asset), time.Now().Add(bridge.DepositAddressLead)).Scan(&row.Address, &row.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (h earnRoutes) mintTronDepositAddress(ctx context.Context, wallet solana.PublicKey, userID string) (bridge.DepositAddress, error) {
	minted, err := h.Relay.DepositAddress(ctx, bridge.Tron, wallet.String())
	var refused *bridge.RefusedOrder
	if errors.As(err, &refused) {
		slog.Warn("relay deposit order refused", "wallet", wallet.String(), "err", err)
		return bridge.DepositAddress{}, earnError(http.StatusBadGateway, "tron_deposit_unavailable", "Relay returned a deposit address Loyal cannot use. Please try again.")
	}
	if err != nil {
		return bridge.DepositAddress{}, tronDepositUnavailable("relay deposit address", err)
	}
	tag, err := h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.relay_deposit_addresses (address, solana_wallet, app_user_id, chain, asset, quote_request_id, expires_at)
VALUES ($1, $2, $3::text::uuid, $4, $5, $6, $7)
ON CONFLICT (address) DO NOTHING`, minted.Address, wallet.String(), userID, string(bridge.Tron.Chain), string(bridge.Tron.Asset), minted.RequestID, minted.ExpiresAt)
	if err != nil {
		return bridge.DepositAddress{}, tronDepositUnavailable("tron deposit insert", err)
	}
	if tag.RowsAffected() == 0 {
		slog.Warn("relay reissued a stored deposit address", "wallet", wallet.String(), "address", minted.Address)
		return bridge.DepositAddress{}, earnError(http.StatusBadGateway, "tron_deposit_unavailable", "Relay returned a deposit address Loyal cannot use. Please try again.")
	}
	return minted, nil
}
