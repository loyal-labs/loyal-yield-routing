package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
)

// Account positions follow docs.relay.link
// references/protocol/contracts/solana-depository.md (DepositToken). Relay
// names the same state, vault and lookup table in every live quote (checked
// 2026-11-03 on every cell of Destinations); the program trusts the accounts
// it is handed, so each is pinned.
const (
	relayDepositoryProgram = "99vQwtBwYtrqqD9YSXbdum3KBdxPAVxYTaQ3cfnJSrN2"
	relayDepositoryState   = "Dodg2HifwU8rmaVVyMyUZDGTRbqAJTyVYxXPwcbNpBKc"
	relayDepositoryVault   = "7uTT8Xi5RWXzy7h9XL244GRgEycDYDhLjr3ZyNdXi8pZ"
	relayLookupTable       = "Hm9fUgcn7qwDaiNTFiGh6pNtVATgnaRcmK6Bbx6EMZfP"
	depositTokenSelector   = "0b9c60da27a3b413"
	OutSlippageBps         = 100
	outMinFloorBps         = 200
	outHalfFloorPercent    = 50
	outFeeAllowanceKeepPPM = 986000
	outAllowanceUSD        = 4
	usdcDecimals           = 6
	SolanaRelayName        = "solana"
	OutFeeBps              = 40
)

const (
	OutConfirmWindow   = 120 * time.Second
	OutSubmittedWindow = 10 * time.Minute
)

var ErrAppFeeUnset = errors.New("relay app fee recipient is not configured")

type OutRequest struct {
	Chain     Chain
	Asset     Asset
	AmountRaw *big.Int
	Sender    solana.PublicKey
	Recipient string
}

type OutKey struct {
	Pubkey     string
	IsSigner   bool
	IsWritable bool
}

type OutInstruction struct {
	ProgramID string
	Keys      []OutKey
	Data      string
}

type OutQuote struct {
	RequestID       string
	Instructions    []OutInstruction
	LookupTables    []string
	AmountInRaw     *big.Int
	AmountOutRaw    *big.Int
	AmountOutMinRaw *big.Int
	Rate            string
	AmountInUSD     string
	AmountOutUSD    string
	NetworkFeeUSD   string
	AppFeeRaw       *big.Int
	ETASeconds      *int64
}

type OutBelowMinimum struct{ MinUSDC uint64 }

func (e *OutBelowMinimum) Error() string {
	return fmt.Sprintf("below the %d USDC minimum for this destination", e.MinUSDC)
}

type QuoteRefused struct{ Code string }

func (e *QuoteRefused) Error() string { return "relay refused the quote: " + e.Code }

var relayErrorCode = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

type relayOutQuote struct {
	RequestID string `json:"requestId"`
	Steps     []struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		RequestID string `json:"requestId"`
		Items     []struct {
			Data struct {
				Instructions []struct {
					Keys []struct {
						Pubkey     string `json:"pubkey"`
						IsSigner   bool   `json:"isSigner"`
						IsWritable bool   `json:"isWritable"`
					} `json:"keys"`
					ProgramID string `json:"programId"`
					Data      string `json:"data"`
				} `json:"instructions"`
				AddressLookupTableAddresses []string `json:"addressLookupTableAddresses"`
			} `json:"data"`
		} `json:"items"`
	} `json:"steps"`
	Fees struct {
		Gas relayCurrencyAmount `json:"gas"`
		App relayCurrencyAmount `json:"app"`
	} `json:"fees"`
	Details struct {
		Sender            string              `json:"sender"`
		Recipient         string              `json:"recipient"`
		TimeEstimate      *float64            `json:"timeEstimate"`
		CurrencyIn        relayCurrencyAmount `json:"currencyIn"`
		CurrencyOut       relayCurrencyAmount `json:"currencyOut"`
		SlippageTolerance struct {
			Total string `json:"total"`
		} `json:"slippageTolerance"`
	} `json:"details"`
	Protocol struct {
		V2 struct {
			OrderID   string `json:"orderId"`
			OrderData struct {
				Inputs []struct {
					Payment struct {
						ChainID  string `json:"chainId"`
						Currency string `json:"currency"`
						Amount   string `json:"amount"`
					} `json:"payment"`
					Refunds []struct {
						ChainID       string `json:"chainId"`
						Recipient     string `json:"recipient"`
						Currency      string `json:"currency"`
						MinimumAmount string `json:"minimumAmount"`
					} `json:"refunds"`
				} `json:"inputs"`
				Output struct {
					ChainID  string            `json:"chainId"`
					Calls    []json.RawMessage `json:"calls"`
					Payments []struct {
						Recipient     string `json:"recipient"`
						Currency      string `json:"currency"`
						MinimumAmount string `json:"minimumAmount"`
					} `json:"payments"`
				} `json:"output"`
			} `json:"orderData"`
			PaymentDetails struct {
				ChainID    string `json:"chainId"`
				Depository string `json:"depository"`
				Currency   string `json:"currency"`
				Amount     string `json:"amount"`
			} `json:"paymentDetails"`
		} `json:"v2"`
	} `json:"protocol"`
}

func resolveOut(req OutRequest) (Destination, DestinationToken, error) {
	dest, ok := LookupDestination(req.Chain)
	if !ok {
		return Destination{}, DestinationToken{}, &RequestError{fmt.Sprintf("unsupported chain %q", req.Chain)}
	}
	tok, ok := dest.Token(req.Asset)
	if !ok {
		return Destination{}, DestinationToken{}, &RequestError{fmt.Sprintf("%s has no %s", req.Chain, req.Asset)}
	}
	if req.AmountRaw == nil || req.AmountRaw.Sign() <= 0 || !req.AmountRaw.IsUint64() {
		return Destination{}, DestinationToken{}, &RequestError{"amount must be positive and fit 64 bits"}
	}
	if min := dest.MinUSDC[req.Asset]; min > 0 && req.AmountRaw.Cmp(new(big.Int).Mul(new(big.Int).SetUint64(min), tenPow(usdcDecimals))) < 0 {
		return Destination{}, DestinationToken{}, &OutBelowMinimum{MinUSDC: min}
	}
	if req.Sender.IsZero() {
		return Destination{}, DestinationToken{}, &RequestError{"empty sender"}
	}
	canonical, err := ParseRecipient(dest.Kind, req.Recipient)
	if err != nil || canonical != req.Recipient {
		return Destination{}, DestinationToken{}, &RequestError{"recipient is not canonical for " + string(req.Chain)}
	}
	return dest, tok, nil
}

func (r *Relay) QuoteOut(ctx context.Context, req OutRequest) (OutQuote, error) {
	dest, tok, err := resolveOut(req)
	if err != nil {
		return OutQuote{}, err
	}
	if r.AppFeeRecipient == "" {
		return OutQuote{}, ErrAppFeeUnset
	}
	body, err := json.Marshal(map[string]any{
		"user":                req.Sender.String(),
		"recipient":           req.Recipient,
		"refundTo":            req.Sender.String(),
		"originChainId":       SolanaRelayChainID,
		"destinationChainId":  dest.RelayChainID,
		"originCurrency":      SolanaUSDCMint,
		"destinationCurrency": tok.Address,
		"amount":              req.AmountRaw.String(),
		"tradeType":           "EXACT_INPUT",
		"slippageTolerance":   strconv.Itoa(OutSlippageBps),
		"appFees":             []map[string]string{{"recipient": r.AppFeeRecipient, "fee": strconv.Itoa(OutFeeBps)}},
	})
	if err != nil {
		return OutQuote{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/quote/v2", bytes.NewReader(body))
	if err != nil {
		return OutQuote{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	raw, err := r.do(hreq)
	var failed *HTTPError
	if errors.As(err, &failed) && failed.Status >= 400 && failed.Status < 500 && failed.Status != http.StatusTooManyRequests && failed.Status != http.StatusUnauthorized && failed.Status != http.StatusForbidden {
		var body struct {
			ErrorCode string `json:"errorCode"`
		}
		_ = json.Unmarshal(failed.Body, &body)
		if !relayErrorCode.MatchString(body.ErrorCode) {
			body.ErrorCode = ""
		}
		return OutQuote{}, &QuoteRefused{Code: body.ErrorCode}
	}
	if err != nil {
		return OutQuote{}, fmt.Errorf("relay quote: %w", err)
	}
	return CheckOutQuote(req, raw)
}

// CheckOutQuote parses Relay's response, which is untrusted. The user's wallet
// signs the one instruction it returns, so every account in it is checked.
func CheckOutQuote(req OutRequest, raw []byte) (OutQuote, error) {
	dest, tok, err := resolveOut(req)
	if err != nil {
		return OutQuote{}, err
	}
	var rq relayOutQuote
	if err := json.Unmarshal(raw, &rq); err != nil {
		return OutQuote{}, fmt.Errorf("quote: %w", err)
	}
	sender := req.Sender.String()
	d := rq.Details
	if rq.RequestID == "" {
		return OutQuote{}, fmt.Errorf("quote: empty requestId")
	}
	if d.Sender != sender {
		return OutQuote{}, fmt.Errorf("quote: details.sender %q is not the user's wallet", d.Sender)
	}
	if d.Recipient != req.Recipient {
		return OutQuote{}, fmt.Errorf("quote: details.recipient %q is not the requested recipient", d.Recipient)
	}
	if d.CurrencyIn.Currency.ChainID != SolanaRelayChainID || d.CurrencyIn.Currency.Address != SolanaUSDCMint {
		return OutQuote{}, fmt.Errorf("quote: details.currencyIn is not Solana USDC")
	}
	in, ok := parseAmount(d.CurrencyIn.Amount)
	if !ok || in.Cmp(req.AmountRaw) != 0 {
		return OutQuote{}, fmt.Errorf("quote: details.currencyIn.amount %q is not the requested amount", d.CurrencyIn.Amount)
	}
	if d.CurrencyOut.Currency.ChainID != dest.RelayChainID || !sameAddress(dest.Kind, d.CurrencyOut.Currency.Address, tok.Address) {
		return OutQuote{}, fmt.Errorf("quote: details.currencyOut is not %s on %s", req.Asset, req.Chain)
	}
	out, ok := parseAmount(d.CurrencyOut.Amount)
	if !ok || out.Sign() <= 0 {
		return OutQuote{}, fmt.Errorf("quote: details.currencyOut.amount %q is not positive", d.CurrencyOut.Amount)
	}
	if floor := outFloor(in, tok.Decimals); out.Cmp(floor) < 0 {
		return OutQuote{}, fmt.Errorf("quote: details.currencyOut.amount %s is below the floor %s for %s in", out, floor, in)
	}
	outMin, ok := parseAmount(d.CurrencyOut.MinimumAmount)
	if !ok || outMin.Sign() <= 0 || outMin.Cmp(out) > 0 {
		return OutQuote{}, fmt.Errorf("quote: details.currencyOut.minimumAmount %q is invalid", d.CurrencyOut.MinimumAmount)
	}
	floor := new(big.Int).Mul(out, big.NewInt(10000-outMinFloorBps))
	if new(big.Int).Mul(outMin, big.NewInt(10000)).Cmp(floor) < 0 {
		return OutQuote{}, fmt.Errorf("quote: details.currencyOut.minimumAmount %s is more than %d bps below the output", outMin, outMinFloorBps)
	}
	if d.SlippageTolerance.Total != strconv.Itoa(OutSlippageBps) {
		return OutQuote{}, fmt.Errorf("quote: details.slippageTolerance.total %q is not %d", d.SlippageTolerance.Total, OutSlippageBps)
	}
	app := rq.Fees.App
	if app.Currency.ChainID != SolanaRelayChainID || app.Currency.Address != SolanaUSDCMint {
		return OutQuote{}, fmt.Errorf("quote: fees.app is not in Solana USDC")
	}
	appFee, ok := parseAmount(app.Amount)
	if !ok || !withinOneUnit(appFee, in, OutFeeBps) {
		return OutQuote{}, fmt.Errorf("quote: fees.app.amount %q is not %d bps of the requested amount", app.Amount, OutFeeBps)
	}

	mint := solana.MustPublicKeyFromBase58(SolanaUSDCMint)
	userATA, _, err := solana.FindAssociatedTokenAddress(req.Sender, mint)
	if err != nil {
		return OutQuote{}, err
	}
	vault := solana.MustPublicKeyFromBase58(relayDepositoryVault)
	vaultATA, _, err := solana.FindAssociatedTokenAddress(vault, mint)
	if err != nil {
		return OutQuote{}, err
	}
	if len(rq.Steps) != 1 {
		return OutQuote{}, fmt.Errorf("quote: %d steps, want 1", len(rq.Steps))
	}
	step := rq.Steps[0]
	switch {
	case step.ID != "deposit":
		return OutQuote{}, fmt.Errorf("quote: steps[0].id %q is not deposit", step.ID)
	case step.Kind != "transaction":
		return OutQuote{}, fmt.Errorf("quote: steps[0].kind %q is not transaction", step.Kind)
	case step.RequestID != rq.RequestID:
		return OutQuote{}, fmt.Errorf("quote: steps[0].requestId differs from requestId")
	case len(step.Items) != 1:
		return OutQuote{}, fmt.Errorf("quote: steps[0] has %d items, want 1", len(step.Items))
	case len(step.Items[0].Data.Instructions) != 1:
		return OutQuote{}, fmt.Errorf("quote: steps[0] has %d instructions, want 1", len(step.Items[0].Data.Instructions))
	}
	item := step.Items[0].Data
	ix := item.Instructions[0]
	if ix.ProgramID != relayDepositoryProgram {
		return OutQuote{}, fmt.Errorf("quote: instruction program %s is not Relay's depository", ix.ProgramID)
	}
	want := [...]struct {
		pubkey           string
		signer, writable bool
	}{
		{relayDepositoryState, false, false},
		{sender, true, true},
		{sender, false, false},
		{relayDepositoryVault, false, false},
		{SolanaUSDCMint, false, false},
		{userATA.String(), false, true},
		{vaultATA.String(), false, true},
		{solana.TokenProgramID.String(), false, false},
		{solana.SPLAssociatedTokenAccountProgramID.String(), false, false},
		{solana.SystemProgramID.String(), false, false},
	}
	if len(ix.Keys) != len(want) {
		return OutQuote{}, fmt.Errorf("quote: instruction has %d keys, want %d", len(ix.Keys), len(want))
	}
	for i, w := range want {
		k := ix.Keys[i]
		if k.Pubkey != w.pubkey || k.IsSigner != w.signer || k.IsWritable != w.writable {
			return OutQuote{}, fmt.Errorf("quote: instruction key %d is not the expected account", i)
		}
	}
	data, err := hex.DecodeString(ix.Data)
	if err != nil || len(data) != 48 || hex.EncodeToString(data[:8]) != depositTokenSelector {
		return OutQuote{}, fmt.Errorf("quote: instruction data is not deposit_token(amount, id)")
	}
	if binary.LittleEndian.Uint64(data[8:16]) != req.AmountRaw.Uint64() {
		return OutQuote{}, fmt.Errorf("quote: instruction amount is not the requested amount")
	}
	if len(item.AddressLookupTableAddresses) > 1 || (len(item.AddressLookupTableAddresses) == 1 && item.AddressLookupTableAddresses[0] != relayLookupTable) {
		return OutQuote{}, fmt.Errorf("quote: lookup tables %v are not Relay's", item.AddressLookupTableAddresses)
	}

	if err := checkOutOrder(rq, req, dest, tok, data[16:], outMin); err != nil {
		return OutQuote{}, fmt.Errorf("quote: order: %w", err)
	}

	q := OutQuote{
		RequestID:       rq.RequestID,
		LookupTables:    item.AddressLookupTableAddresses,
		AmountInRaw:     in,
		AmountOutRaw:    out,
		AmountOutMinRaw: outMin,
		Rate:            outRate(in, out, tok.Decimals),
		AmountInUSD:     plainDecimalOrEmpty(d.CurrencyIn.AmountUSD),
		AmountOutUSD:    plainDecimalOrEmpty(d.CurrencyOut.AmountUSD),
		NetworkFeeUSD:   plainDecimalOrEmpty(rq.Fees.Gas.AmountUSD),
		AppFeeRaw:       appFee,
	}
	if d.TimeEstimate != nil && *d.TimeEstimate >= 0 {
		eta := int64(math.Ceil(*d.TimeEstimate))
		q.ETASeconds = &eta
	}
	oi := OutInstruction{ProgramID: ix.ProgramID, Data: ix.Data}
	for _, k := range ix.Keys {
		oi.Keys = append(oi.Keys, OutKey{Pubkey: k.Pubkey, IsSigner: k.IsSigner, IsWritable: k.IsWritable})
	}
	q.Instructions = []OutInstruction{oi}
	return q, nil
}

func checkOutOrder(rq relayOutQuote, req OutRequest, dest Destination, tok DestinationToken, depositID []byte, outMin *big.Int) error {
	p := rq.Protocol.V2
	if id, err := hex.DecodeString(strings.TrimPrefix(p.OrderID, "0x")); err != nil || !bytes.Equal(id, depositID) {
		return fmt.Errorf("orderId is not the deposit id")
	}
	pay := p.PaymentDetails
	amount := req.AmountRaw.String()
	if pay.Depository != relayDepositoryProgram || pay.Currency != SolanaUSDCMint || pay.Amount != amount {
		return fmt.Errorf("paymentDetails are not the requested USDC into Relay's depository")
	}
	if len(p.OrderData.Inputs) != 1 {
		return fmt.Errorf("%d inputs, want 1", len(p.OrderData.Inputs))
	}
	input := p.OrderData.Inputs[0]
	if input.Payment.Currency != SolanaUSDCMint || input.Payment.Amount != amount {
		return fmt.Errorf("input payment is not the requested USDC")
	}
	if len(input.Refunds) == 0 || input.Refunds[0].Recipient != req.Sender.String() || input.Refunds[0].Currency != SolanaUSDCMint {
		return fmt.Errorf("first refund is not the user's USDC on Solana")
	}
	for i, refund := range input.Refunds {
		toUser := refund.ChainID == SolanaRelayName && refund.Recipient == req.Sender.String()
		toRecipient := refund.ChainID == dest.RelayName && sameAddress(dest.Kind, refund.Recipient, req.Recipient)
		if !toUser && !toRecipient {
			return fmt.Errorf("refund %d on %s to %s is neither the user's wallet nor the requested recipient", i, refund.ChainID, refund.Recipient)
		}
	}
	if p.OrderData.Output.ChainID != dest.RelayName {
		return fmt.Errorf("output chain %q is not %s", p.OrderData.Output.ChainID, dest.RelayName)
	}
	if len(p.OrderData.Output.Calls) != 0 {
		return fmt.Errorf("output runs %d calls, want none", len(p.OrderData.Output.Calls))
	}
	if len(p.OrderData.Output.Payments) != 1 {
		return fmt.Errorf("%d output payments, want 1", len(p.OrderData.Output.Payments))
	}
	payment := p.OrderData.Output.Payments[0]
	if !sameAddress(dest.Kind, payment.Recipient, req.Recipient) || !sameAddress(dest.Kind, payment.Currency, tok.Address) {
		return fmt.Errorf("output payment is not the requested token to the requested recipient")
	}
	if min, ok := parseAmount(payment.MinimumAmount); !ok || min.Cmp(outMin) != 0 {
		return fmt.Errorf("output minimumAmount %q differs from the quote's", payment.MinimumAmount)
	}
	return nil
}

func withinOneUnit(fee, amount *big.Int, bps int64) bool {
	diff := new(big.Int).Mul(fee, big.NewInt(10000))
	diff.Sub(diff, new(big.Int).Mul(amount, big.NewInt(bps)))
	return diff.CmpAbs(big.NewInt(10000)) <= 0
}

func sameAddress(kind AddressKind, a, b string) bool {
	if kind == AddressEVM {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func plainDecimalOrEmpty(s string) string {
	if plainDecimal.MatchString(s) {
		return s
	}
	return ""
}

func tenPow(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }

func scaleUSDC(in *big.Int, decimals uint8) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(in, tenPow(int64(decimals))), tenPow(usdcDecimals))
}

func outFloor(in *big.Int, decimals uint8) *big.Int {
	scaled := scaleUSDC(in, decimals)
	half := new(big.Int).Div(new(big.Int).Mul(scaled, big.NewInt(outHalfFloorPercent)), big.NewInt(100))
	afterFees := new(big.Int).Div(new(big.Int).Mul(scaled, big.NewInt(outFeeAllowanceKeepPPM)), big.NewInt(1_000_000))
	afterFees.Sub(afterFees, new(big.Int).Mul(big.NewInt(outAllowanceUSD), tenPow(int64(decimals))))
	if afterFees.Cmp(half) > 0 {
		return afterFees
	}
	return half
}

func outRate(in, out *big.Int, decimals uint8) string {
	return new(big.Rat).SetFrac(new(big.Int).Mul(out, tenPow(usdcDecimals)), new(big.Int).Mul(in, tenPow(int64(decimals)))).FloatString(6)
}
