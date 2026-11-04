package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

const (
	bridgeDestinationsPath = "/api/funding/bridge/destinations"
	bridgeOutQuotePath     = "/api/funding/bridge/out/quote"
	bridgeRecipientsPath   = "/api/funding/bridge/out/recipients"
	bridgeRecentsLimit     = 5
)

type BridgeDestinationToken struct {
	Asset    bridge.Asset `json:"asset"`
	Symbol   string       `json:"symbol"`
	Name     string       `json:"name"`
	Icon     string       `json:"icon"`
	Address  string       `json:"address"`
	Decimals uint8        `json:"decimals"`
	MinUSDC  uint64       `json:"minUsdc" doc:"Smallest whole-USDC amount this token is quoted for; 0 is no minimum"`
}

type BridgeDestination struct {
	Chain       bridge.Chain             `json:"chain"`
	Name        string                   `json:"name"`
	Icon        string                   `json:"icon"`
	AddressKind bridge.AddressKind       `json:"addressKind" enum:"evm,tron"`
	ExplorerTx  string                   `json:"explorerTx" doc:"Transaction URL prefix; append the hash"`
	PopularRank int                      `json:"popularRank" doc:"1 is first among the popular chains; 0 is not popular"`
	Tokens      []BridgeDestinationToken `json:"tokens"`
}

type BridgeDestinationsResponse struct {
	Destinations []BridgeDestination `json:"destinations"`
}

type BridgeOutQuoteRequest struct {
	Chain     bridge.Chain `json:"chain"`
	Asset     bridge.Asset `json:"asset"`
	AmountRaw string       `json:"amountRaw" doc:"Solana USDC to send, in 6-decimal units"`
	Recipient string       `json:"recipient"`
}

type BridgeOutKey struct {
	Pubkey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}

type BridgeOutInstruction struct {
	ProgramID string         `json:"programId"`
	Keys      []BridgeOutKey `json:"keys"`
	Data      string         `json:"data" doc:"Hex"`
}

type BridgeOutQuoteResponse struct {
	RequestID       string                 `json:"requestId"`
	Instructions    []BridgeOutInstruction `json:"instructions"`
	LookupTables    []string               `json:"lookupTables"`
	AmountOutRaw    string                 `json:"amountOutRaw"`
	AmountOutMinRaw string                 `json:"amountOutMinRaw"`
	Rate            string                 `json:"rate" doc:"Output over input, computed by the server"`
	AmountInUSD     string                 `json:"amountInUsd" doc:"Display only, passed through from Relay"`
	AmountOutUSD    string                 `json:"amountOutUsd" doc:"Display only, passed through from Relay"`
	NetworkFeeUSD   string                 `json:"networkFeeUsd" doc:"Display only, passed through from Relay"`
	LoyalFeeRaw     string                 `json:"loyalFeeRaw" doc:"Loyal's fee in raw USDC units, the amount the guard checked, taken from the amount received"`
	LoyalFeeBps     int                    `json:"loyalFeeBps"`
	ETASeconds      *int64                 `json:"etaSeconds"`
}

type BridgeRecipient struct {
	Address string       `json:"address"`
	Chain   bridge.Chain `json:"chain" doc:"The chain of the latest send to this address"`
	Sends   int          `json:"sends"`
}

type BridgeRecipientsResponse struct {
	Recipients []BridgeRecipient `json:"recipients"`
}

type bridgeRecipientsInput struct {
	SessionInput
	Chain string `query:"chain" required:"false" doc:"Returns recipients of this chain's address kind"`
}

type bridgeDestinationsOutput struct{ Body BridgeDestinationsResponse }
type bridgeRecipientsOutput struct{ Body BridgeRecipientsResponse }
type bridgeOutQuoteOutput struct{ Body BridgeOutQuoteResponse }

func (h earnRoutes) registerBridgeOut(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-bridge-destinations",
		Method:      http.MethodGet,
		Path:        bridgeDestinationsPath,
		Summary:     "The chains and stablecoins the signed-in wallet's Solana USDC can be sent to",
		Responses:   errorResponses[routeError](api, 401, 503),
	}, h.bridgeDestinations)
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-bridge-out-quote",
		Method:      http.MethodPost,
		Path:        bridgeOutQuotePath,
		Summary:     "Quote Solana USDC from the signed-in wallet to a stablecoin on another chain, with the deposit instruction to sign",
		Responses:   errorResponses[routeError](api, 400, 401, 429, 503),
	}, h.bridgeOutQuote)
	documentJSONBody[BridgeOutQuoteRequest](api, bridgeOutQuotePath)
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-bridge-out-recipients",
		Method:      http.MethodGet,
		Path:        bridgeRecipientsPath,
		Summary:     "The signed-in wallet's most recent bridge-out recipients of a chain's address kind",
		Responses:   errorResponses[routeError](api, 400, 401, 503),
	}, h.bridgeOutRecipients)
}

func bridgeOutUnavailable(step string, err error) error {
	return earnUnavailable(step, err, "bridge_out_unavailable", "Sending USDC to another chain is unavailable. Please try again.")
}

func (h earnRoutes) bridgeDestinations(ctx context.Context, in *sessionRequest) (*bridgeDestinationsOutput, error) {
	if _, err := h.evmDepositWallet(in.SessionInput); err != nil {
		return nil, err
	}
	out := &bridgeDestinationsOutput{Body: BridgeDestinationsResponse{Destinations: make([]BridgeDestination, 0, len(bridge.Destinations))}}
	for _, d := range bridge.Destinations {
		dest := BridgeDestination{Chain: d.Chain, Name: d.Name, Icon: d.Icon, AddressKind: d.Kind, ExplorerTx: d.ExplorerTx, PopularRank: d.PopularRank, Tokens: make([]BridgeDestinationToken, 0, len(d.Tokens))}
		for _, t := range d.Tokens {
			info := bridge.Assets[t.Asset]
			dest.Tokens = append(dest.Tokens, BridgeDestinationToken{Asset: t.Asset, Symbol: info.Symbol, Name: info.Name, Icon: info.Icon, Address: t.Address, Decimals: t.Decimals, MinUSDC: d.MinUSDC[t.Asset]})
		}
		out.Body.Destinations = append(out.Body.Destinations, dest)
	}
	return out, nil
}

func (h earnRoutes) bridgeOutQuote(ctx context.Context, in *bridgeQuoteInput) (*bridgeOutQuoteOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	var body BridgeOutQuoteRequest
	if json.Unmarshal(in.Raw.data, &body) != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "chain, asset, amountRaw and recipient required.")
	}
	amount, ok := new(big.Int).SetString(body.AmountRaw, 10)
	if !ok || amount.Sign() <= 0 || !amount.IsUint64() {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "amountRaw must be a positive decimal integer.")
	}
	dest, ok := bridge.LookupDestination(body.Chain)
	if !ok {
		return nil, earnError(http.StatusBadRequest, "invalid_quote_request", "That chain is not offered.")
	}
	if _, ok := dest.Token(body.Asset); !ok {
		return nil, earnError(http.StatusBadRequest, "invalid_quote_request", "That asset is not offered on this chain.")
	}
	recipient, err := bridge.ParseRecipient(dest.Kind, body.Recipient)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_recipient", "That is not a valid address on "+dest.Name+".")
	}
	if !h.bridgeQuotes.get(limiterKey{"bridge-out-quote", "wallet", wallet.String()}, bridgeQuoteBudget, time.Now()).Allow() {
		return nil, earnError(http.StatusTooManyRequests, "rate_limited", "Too many quotes. Please wait a minute.")
	}
	q, err := h.Relay.QuoteOut(ctx, bridge.OutRequest{Chain: body.Chain, Asset: body.Asset, AmountRaw: amount, Sender: wallet, Recipient: recipient})
	var refusedRequest *bridge.RequestError
	var refused *bridge.QuoteRefused
	var belowMinimum *bridge.OutBelowMinimum
	switch {
	case errors.As(err, &belowMinimum):
		return nil, earnError(http.StatusBadRequest, "bridge_below_minimum", fmt.Sprintf("The smallest amount to send there is %d USDC.", belowMinimum.MinUSDC))
	case errors.As(err, &refusedRequest):
		return nil, earnError(http.StatusBadRequest, "invalid_quote_request", refusedRequest.Error())
	case errors.As(err, &refused):
		message := "Relay cannot quote this transfer."
		if refused.Code != "" {
			message += " (" + refused.Code + ")"
		}
		return nil, earnError(http.StatusBadRequest, "bridge_quote_refused", message)
	case err != nil:
		return nil, bridgeOutUnavailable("relay out quote", err)
	}
	if err := insertBridgeOut(ctx, h.YieldDB, wallet, recipient, body.Chain, body.Asset, q); err != nil {
		return nil, bridgeOutUnavailable("bridge out transfer insert", err)
	}
	out := &bridgeOutQuoteOutput{Body: BridgeOutQuoteResponse{
		RequestID:       q.RequestID,
		LookupTables:    append([]string{}, q.LookupTables...),
		AmountOutRaw:    q.AmountOutRaw.String(),
		AmountOutMinRaw: q.AmountOutMinRaw.String(),
		Rate:            q.Rate,
		AmountInUSD:     q.AmountInUSD,
		AmountOutUSD:    q.AmountOutUSD,
		NetworkFeeUSD:   q.NetworkFeeUSD,
		LoyalFeeBps:     bridge.OutFeeBps,
		LoyalFeeRaw:     q.AppFeeRaw.String(),
		ETASeconds:      q.ETASeconds,
	}}
	for _, ix := range q.Instructions {
		keys := make([]BridgeOutKey, len(ix.Keys))
		for i, k := range ix.Keys {
			keys[i] = BridgeOutKey{Pubkey: k.Pubkey, IsSigner: k.IsSigner, IsWritable: k.IsWritable}
		}
		out.Body.Instructions = append(out.Body.Instructions, BridgeOutInstruction{ProgramID: ix.ProgramID, Keys: keys, Data: ix.Data})
	}
	return out, nil
}

func insertBridgeOut(ctx context.Context, db *pgxpool.Pool, wallet solana.PublicKey, recipient string, chain bridge.Chain, asset bridge.Asset, q bridge.OutQuote) error {
	tag, err := db.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers (request_id, solana_wallet, counterparty, direction, chain, asset, amount_in_raw, amount_out_raw)
VALUES ($1, $2, $3, 'out', $4, $5, $6::text::numeric, $7::text::numeric)
ON CONFLICT (request_id) DO NOTHING`, q.RequestID, wallet.String(), recipient, string(chain), string(asset), q.AmountInRaw.String(), q.AmountOutRaw.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("request id already has a row")
	}
	return nil
}

func (h earnRoutes) bridgeOutRecipients(ctx context.Context, in *bridgeRecipientsInput) (*bridgeRecipientsOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	dest, ok := bridge.LookupDestination(bridge.Chain(in.Chain))
	if !ok {
		return nil, earnError(http.StatusBadRequest, "invalid_quote_request", "That chain is not offered.")
	}
	var sameKind []string
	for _, d := range bridge.Destinations {
		if d.Kind == dest.Kind {
			sameKind = append(sameKind, string(d.Chain))
		}
	}
	rows, err := h.YieldDB.Query(ctx, `SELECT counterparty, (array_agg(chain ORDER BY submitted_at DESC))[1], count(*)
FROM loyal_yield.bridge_transfers
WHERE solana_wallet = $1 AND direction = 'out' AND state IN ('submitted', 'filled') AND chain = ANY($2)
GROUP BY counterparty ORDER BY max(submitted_at) DESC LIMIT $3`, wallet.String(), sameKind, bridgeRecentsLimit)
	if err != nil {
		return nil, bridgeOutUnavailable("bridge out recipients read", err)
	}
	recipients, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BridgeRecipient, error) {
		var r BridgeRecipient
		err := row.Scan(&r.Address, &r.Chain, &r.Sends)
		return r, err
	})
	if err != nil {
		return nil, bridgeOutUnavailable("bridge out recipients read", err)
	}
	return &bridgeRecipientsOutput{Body: BridgeRecipientsResponse{Recipients: recipients}}, nil
}
