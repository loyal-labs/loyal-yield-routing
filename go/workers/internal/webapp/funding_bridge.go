package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"golang.org/x/sync/errgroup"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

var earnBridgeFamily = earnFamily{
	name: "evm bridge",
	yieldGrants: []Grant{
		TableGrant("loyal_yield.evm_deposit_routes", "SELECT"),
		TableGrant("loyal_yield.bridge_transfers", "SELECT"),
		TableGrant("loyal_yield.bridge_transfers", "INSERT"),
		TableGrant("loyal_yield.bridge_transfers", "UPDATE"),
	},
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Privy != nil && h.Privy.AppID != "" && h.Privy.AppSecret != "" && h.Refusals != nil
	},
	register: earnRoutes.registerBridge,
}

const (
	bridgeBalancesPath  = "/api/funding/bridge/balances"
	bridgeQuotePath     = "/api/funding/bridge/quote"
	bridgeTransfersPath = "/api/funding/bridge/transfers"
	bridgeConfirmPath   = "/api/funding/bridge/transfers/{requestId}/confirm"
	bridgeSubmitPath    = "/api/funding/bridge/transfers/{requestId}/submit"
	bridgeTransferLimit = 20
)

var bridgeQuoteBudget = budget{every: 15 * time.Second, burst: 12}

type BridgeBalance struct {
	Chain     bridge.Chain `json:"chain"`
	Asset     bridge.Asset `json:"asset"`
	AmountRaw string       `json:"amountRaw"`
	Decimals  uint8        `json:"decimals"`
}

type BridgeBalancesResponse struct {
	Balances []BridgeBalance `json:"balances"`
}

type BridgeQuoteRequest struct {
	Chain     bridge.Chain `json:"chain"`
	Asset     bridge.Asset `json:"asset"`
	AmountRaw string       `json:"amountRaw"`
}

type BridgeCall struct {
	To    string `json:"to"`
	Data  string `json:"data"`
	Value string `json:"value"`
}

type BridgeQuoteResponse struct {
	RequestID       string       `json:"requestId"`
	ChainID         int64        `json:"chainId"`
	Calls           []BridgeCall `json:"calls"`
	AmountOutRaw    string       `json:"amountOutRaw"`
	AmountOutMinRaw string       `json:"amountOutMinRaw"`
	RelayerFeeUSD   string       `json:"relayerFeeUsd"`
	ETASeconds      *int64       `json:"etaSeconds"`
}

type BridgeSubmitRequest struct {
	OriginTx string `json:"originTx"`
}

type BridgeTransfer struct {
	RequestID     string               `json:"requestId"`
	Chain         bridge.Chain         `json:"chain"`
	Asset         bridge.Asset         `json:"asset"`
	AmountInRaw   string               `json:"amountInRaw"`
	AmountOutRaw  string               `json:"amountOutRaw"`
	Direction     bridge.Direction     `json:"direction" enum:"in,out"`
	State         bridge.TransferState `json:"state" enum:"quoted,submitted,filled,refunded,failed"`
	OriginTx      *string              `json:"originTx"`
	DestinationTx *string              `json:"destinationTx"`
	RefundTx      *string              `json:"refundTx"`
	QuotedAt      time.Time            `json:"quotedAt"`
	SubmittedAt   *time.Time           `json:"submittedAt"`
	SettledAt     *time.Time           `json:"settledAt"`
}

type BridgeTransfersResponse struct {
	Transfers []BridgeTransfer `json:"transfers"`
}

type bridgeBalancesOutput struct{ Body BridgeBalancesResponse }
type bridgeQuoteOutput struct{ Body BridgeQuoteResponse }
type bridgeTransferOutput struct{ Body BridgeTransfer }
type bridgeTransfersOutput struct{ Body BridgeTransfersResponse }

type bridgeQuoteInput struct {
	SessionInput
	Raw rawJSON
}

type bridgeTransfersInput struct {
	SessionInput
	Direction string `query:"direction" required:"false" doc:"in (the default) or out"`
}

type bridgeConfirmInput struct {
	SessionInput
	RequestID string `path:"requestId" doc:"Relay request id from the quote"`
}

type bridgeSubmitInput struct {
	SessionInput
	RequestID string `path:"requestId" doc:"Relay request id from the quote"`
	Raw       rawJSON
}

func (h earnRoutes) registerBridge(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-bridge-balances",
		Method:      http.MethodGet,
		Path:        bridgeBalancesPath,
		Summary:     "What the signed-in wallet's EVM wallet holds on each supported chain",
		Responses:   errorResponses[routeError](api, 401, 404, 503),
	}, h.bridgeBalances)
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-bridge-quote",
		Method:      http.MethodPost,
		Path:        bridgeQuotePath,
		Summary:     "Quote a Relay bridge from the signed-in wallet's EVM wallet to it",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 429, 503),
	}, h.bridgeQuote)
	documentJSONBody[BridgeQuoteRequest](api, bridgeQuotePath)
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-bridge-transfer-confirm",
		Method:      http.MethodPost,
		Path:        bridgeConfirmPath,
		Summary:     "Record that the user confirmed a quoted bridge, before any send",
		Responses:   errorResponses[routeError](api, 401, 404, 503),
	}, h.bridgeConfirm)
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-bridge-transfer-submit",
		Method:      http.MethodPost,
		Path:        bridgeSubmitPath,
		Summary:     "Record the transaction that sent a quoted bridge",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 503),
	}, h.bridgeSubmit)
	documentJSONBody[BridgeSubmitRequest](api, bridgeSubmitPath)
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-bridge-transfers",
		Method:      http.MethodGet,
		Path:        bridgeTransfersPath,
		Summary:     "The signed-in wallet's latest bridges and their state",
		Responses:   errorResponses[routeError](api, 400, 401, 503),
	}, h.bridgeTransfers)
	h.registerBridgeRefusal(api)
	h.registerBridgeOut(api)
}

func bridgeUnavailable(step string, err error) error {
	return earnUnavailable(step, err, "bridge_unavailable", "Moving funds from other chains is unavailable. Please try again.")
}

func (h earnRoutes) bridgeSender(ctx context.Context, in SessionInput) (solana.PublicKey, string, error) {
	wallet, err := h.evmDepositWallet(in)
	if err != nil {
		return solana.PublicKey{}, "", err
	}
	route, err := h.storedEvmDepositRoute(ctx, wallet)
	if err != nil {
		return solana.PublicKey{}, "", bridgeUnavailable("bridge route read", err)
	}
	if route == nil || route.Kind != RelaySigned {
		return solana.PublicKey{}, "", earnError(http.StatusNotFound, "bridge_wallet_missing", "This wallet has no EVM wallet to move funds from.")
	}
	return wallet, route.Address, nil
}

func (h earnRoutes) bridgeBalances(ctx context.Context, in *sessionRequest) (*bridgeBalancesOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, address, err := h.bridgeSender(ctx, in.SessionInput)
	if err != nil {
		return nil, err
	}
	walletID, err := h.privyWalletID(ctx, "ethereum", address)
	if err != nil {
		return nil, bridgeUnavailable("privy evm wallet lookup", err)
	}
	if walletID == "" {
		return nil, bridgeUnavailable("privy evm wallet lookup", fmt.Errorf("privy has no wallet %s", address))
	}
	queries := bridge.PrivyBalanceQueries()
	pages := make([][]byte, len(queries))
	calls, callsCtx := errgroup.WithContext(ctx)
	for i, query := range queries {
		calls.Go(func() error {
			var raw json.RawMessage
			err := h.privyCall(callsCtx, http.MethodGet, privyAPIBase+"/wallets/"+url.PathEscape(walletID)+"/balance?"+query.Encode(), nil, nil, &raw)
			pages[i] = raw
			return err
		})
	}
	if err := calls.Wait(); err != nil {
		return nil, bridgeUnavailable("privy balances", err)
	}
	balances, err := bridge.ParsePrivyBalances(pages)
	if err != nil {
		return nil, bridgeUnavailable("privy balances", err)
	}
	out := &bridgeBalancesOutput{Body: BridgeBalancesResponse{Balances: make([]BridgeBalance, 0, len(balances))}}
	for _, b := range balances {
		out.Body.Balances = append(out.Body.Balances, BridgeBalance{Chain: b.Chain, Asset: b.Asset, AmountRaw: b.AmountRaw.String(), Decimals: b.Decimals})
	}
	return out, nil
}

func (h earnRoutes) bridgeQuote(ctx context.Context, in *bridgeQuoteInput) (*bridgeQuoteOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, from, err := h.bridgeSender(ctx, in.SessionInput)
	if err != nil {
		return nil, err
	}
	var body BridgeQuoteRequest
	if json.Unmarshal(in.Raw.data, &body) != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "chain, asset and amountRaw required.")
	}
	amount, ok := new(big.Int).SetString(body.AmountRaw, 10)
	if !ok {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "amountRaw must be a decimal integer.")
	}
	if !h.bridgeQuotes.get(limiterKey{"bridge-quote", "wallet", wallet.String()}, bridgeQuoteBudget, time.Now()).Allow() {
		return nil, earnError(http.StatusTooManyRequests, "rate_limited", "Too many quotes. Please wait a minute.")
	}
	q, err := h.Relay.Quote(ctx, bridge.QuoteRequest{Chain: body.Chain, Asset: body.Asset, AmountRaw: amount, From: from, Recipient: wallet.String()})
	var refused *bridge.RequestError
	var low *bridge.BelowMinimum
	switch {
	case errors.As(err, &refused):
		return nil, earnError(http.StatusBadRequest, "invalid_quote_request", refused.Error())
	case errors.As(err, &low):
		return nil, earnError(http.StatusBadRequest, "bridge_below_minimum", fmt.Sprintf("The minimum to move from %s is %d USD.", low.Chain, low.MinUSD))
	case err != nil:
		return nil, bridgeUnavailable("relay quote", err)
	}
	if _, err := h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers (request_id, solana_wallet, counterparty, chain, asset, amount_in_raw, amount_out_raw)
VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7::text::numeric)
ON CONFLICT (request_id) DO NOTHING`, q.RequestID, wallet.String(), from, string(body.Chain), string(body.Asset), q.AmountInRaw.String(), q.AmountOutRaw.String()); err != nil {
		return nil, bridgeUnavailable("bridge transfer insert", err)
	}
	network, _ := bridge.Lookup(body.Chain)
	out := &bridgeQuoteOutput{Body: BridgeQuoteResponse{
		RequestID:       q.RequestID,
		ChainID:         network.ChainID,
		AmountOutRaw:    q.AmountOutRaw.String(),
		AmountOutMinRaw: q.AmountOutMinRaw.String(),
		RelayerFeeUSD:   q.RelayerUSD,
		ETASeconds:      q.ETASeconds,
	}}
	for _, c := range q.Calls {
		out.Body.Calls = append(out.Body.Calls, BridgeCall{To: c.To, Data: c.Data, Value: c.Value})
	}
	return out, nil
}

func (h earnRoutes) bridgeConfirm(ctx context.Context, in *bridgeConfirmInput) (*bridgeTransferOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	tag, err := h.YieldDB.Exec(ctx, `UPDATE loyal_yield.bridge_transfers
SET confirmed_at = coalesce(confirmed_at, clock_timestamp()), updated_at = clock_timestamp()
WHERE request_id = $1 AND solana_wallet = $2 AND state = 'quoted'`, in.RequestID, wallet.String())
	if err != nil {
		return nil, bridgeUnavailable("bridge transfer confirm", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, earnError(http.StatusNotFound, "bridge_transfer_missing", "No quoted transfer with this id for this wallet.")
	}
	t, err := h.bridgeTransfer(ctx, wallet, in.RequestID)
	if err != nil || t == nil {
		return nil, bridgeUnavailable("bridge transfer read", err)
	}
	return &bridgeTransferOutput{Body: *t}, nil
}

func (h earnRoutes) bridgeSubmit(ctx context.Context, in *bridgeSubmitInput) (*bridgeTransferOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	var body BridgeSubmitRequest
	if json.Unmarshal(in.Raw.data, &body) != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "originTx required.")
	}
	t, err := h.bridgeTransfer(ctx, wallet, in.RequestID)
	if err != nil {
		return nil, bridgeUnavailable("bridge transfer read", err)
	}
	if t == nil {
		return nil, earnError(http.StatusNotFound, "bridge_transfer_missing", "No such transfer for this wallet.")
	}
	originTx, ok := bridge.OriginTx(t.Direction, body.OriginTx)
	if !ok {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "originTx must be the transaction hash of the send.")
	}
	if next := t.State.Submit(); next != t.State {
		if _, err := h.YieldDB.Exec(ctx, `UPDATE loyal_yield.bridge_transfers
SET state = $3, origin_tx = $4, submitted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE request_id = $1 AND solana_wallet = $2 AND state = $5`, t.RequestID, wallet.String(), string(next), originTx, string(t.State)); err != nil {
			return nil, bridgeUnavailable("bridge transfer submit", err)
		}
		if t, err = h.bridgeTransfer(ctx, wallet, in.RequestID); err != nil || t == nil {
			return nil, bridgeUnavailable("bridge transfer read", err)
		}
	}
	return &bridgeTransferOutput{Body: *t}, nil
}

func (h earnRoutes) bridgeTransfers(ctx context.Context, in *bridgeTransfersInput) (*bridgeTransfersOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	direction := bridge.Direction(in.Direction)
	switch direction {
	case "":
		direction = bridge.DirectionIn
	case bridge.DirectionIn, bridge.DirectionOut:
	default:
		return nil, earnError(http.StatusBadRequest, "invalid_direction", "direction must be in or out.")
	}
	rows, err := h.YieldDB.Query(ctx, bridgeTransferSelect+` WHERE solana_wallet = $1 AND state <> 'quoted' AND direction = $3 ORDER BY submitted_at DESC NULLS LAST LIMIT $2`, wallet.String(), bridgeTransferLimit, string(direction))
	if err != nil {
		return nil, bridgeUnavailable("bridge transfers read", err)
	}
	transfers, err := pgx.CollectRows(rows, scanBridgeTransfer)
	if err != nil {
		return nil, bridgeUnavailable("bridge transfers read", err)
	}
	return &bridgeTransfersOutput{Body: BridgeTransfersResponse{Transfers: transfers}}, nil
}

const bridgeTransferSelect = `SELECT request_id, chain, asset, amount_in_raw::text, amount_out_raw::text, direction, state, origin_tx, destination_tx, refund_tx, quoted_at, submitted_at, settled_at
FROM loyal_yield.bridge_transfers`

func scanBridgeTransfer(row pgx.CollectableRow) (BridgeTransfer, error) {
	var t BridgeTransfer
	err := row.Scan(&t.RequestID, &t.Chain, &t.Asset, &t.AmountInRaw, &t.AmountOutRaw, &t.Direction, &t.State, &t.OriginTx, &t.DestinationTx, &t.RefundTx, &t.QuotedAt, &t.SubmittedAt, &t.SettledAt)
	return t, err
}

func (h earnRoutes) bridgeTransfer(ctx context.Context, wallet solana.PublicKey, requestID string) (*BridgeTransfer, error) {
	rows, err := h.YieldDB.Query(ctx, bridgeTransferSelect+` WHERE request_id = $1 AND solana_wallet = $2`, requestID, wallet.String())
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanBridgeTransfer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
