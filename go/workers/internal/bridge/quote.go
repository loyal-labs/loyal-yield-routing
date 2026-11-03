package bridge

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
)

type QuoteRequest struct {
	Chain     Chain
	Asset     Asset
	AmountRaw *big.Int
	From      string
	Recipient string
}

// Call is one transaction exactly as Relay returned it: hex Data, decimal
// Value in wei.
type Call struct {
	To    string
	Data  string
	Value string
}

// Quote is a Relay quote that passed CheckQuote. Calls run in order as one
// batch. The USD figures are Relay's own strings, empty when Relay omitted
// them.
type Quote struct {
	RequestID       string
	Calls           []Call
	AmountInRaw     *big.Int
	AmountOutRaw    *big.Int
	AmountOutMinRaw *big.Int
	AmountInUSD     string
	GasUSD          string
	RelayerUSD      string
	ImpactUSD       string
	ImpactPercent   string
	ETASeconds      *int64
}

type relayCurrencyAmount struct {
	Currency struct {
		ChainID int64  `json:"chainId"`
		Address string `json:"address"`
	} `json:"currency"`
	Amount        string `json:"amount"`
	MinimumAmount string `json:"minimumAmount"`
	AmountUSD     string `json:"amountUsd"`
}

type relayQuote struct {
	RequestID string `json:"requestId"`
	Steps     []struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		RequestID string `json:"requestId"`
		Items     []struct {
			Data struct {
				From    string `json:"from"`
				To      string `json:"to"`
				Data    string `json:"data"`
				Value   string `json:"value"`
				ChainID int64  `json:"chainId"`
			} `json:"data"`
		} `json:"items"`
	} `json:"steps"`
	Fees struct {
		Gas     relayCurrencyAmount `json:"gas"`
		Relayer relayCurrencyAmount `json:"relayer"`
	} `json:"fees"`
	Details struct {
		Recipient    string              `json:"recipient"`
		TimeEstimate *float64            `json:"timeEstimate"`
		CurrencyIn   relayCurrencyAmount `json:"currencyIn"`
		CurrencyOut  relayCurrencyAmount `json:"currencyOut"`
		TotalImpact  struct {
			USD     string `json:"usd"`
			Percent string `json:"percent"`
		} `json:"totalImpact"`
	} `json:"details"`
}

const (
	approveSelector       = "095ea7b3"
	depositErc20Selector  = "e8017952"
	depositNativeSelector = "49290c1c"
)

// relayDepository is the contract every live Relay quote deposits into, the
// same address on all five networks (checked 2026-11-02).
const relayDepository = "0x4cd00e387622c35bddb9b4c962c136462338bc31"

type RequestError struct{ reason string }

func (e *RequestError) Error() string { return "quote request: " + e.reason }

func resolve(req QuoteRequest) (Network, Token, error) {
	net, ok := Lookup(req.Chain)
	if !ok {
		return Network{}, Token{}, &RequestError{fmt.Sprintf("unsupported chain %q", req.Chain)}
	}
	tok, ok := net.Token(req.Asset)
	if !ok {
		return Network{}, Token{}, &RequestError{fmt.Sprintf("%s has no %s", req.Chain, req.Asset)}
	}
	if req.AmountRaw == nil || req.AmountRaw.Sign() <= 0 {
		return Network{}, Token{}, &RequestError{"amount must be positive"}
	}
	if !isAddress(req.From) {
		return Network{}, Token{}, &RequestError{"from is not an EVM address"}
	}
	if req.Recipient == "" {
		return Network{}, Token{}, &RequestError{"empty recipient"}
	}
	return net, tok, nil
}

// CheckQuote parses Relay's response, which is untrusted, and refuses any
// quote whose transactions could move the user's funds anywhere but Relay's
// deposit for their own Solana wallet. What binds a quote is that the
// transactions are the user's own, on the right chain, spend no more than the
// quoted amount, deposit exactly that amount of that token for that user, and
// that the quote's recipient and output are the ones requested.
func CheckQuote(req QuoteRequest, raw []byte) (Quote, error) {
	net, tok, err := resolve(req)
	if err != nil {
		return Quote{}, err
	}
	var rq relayQuote
	if err := json.Unmarshal(raw, &rq); err != nil {
		return Quote{}, fmt.Errorf("quote: %w", err)
	}
	d := rq.Details
	if d.Recipient != req.Recipient {
		return Quote{}, fmt.Errorf("quote: details.recipient %q is not the requested recipient", d.Recipient)
	}
	if d.CurrencyOut.Currency.ChainID != SolanaRelayChainID || d.CurrencyOut.Currency.Address != SolanaUSDCMint {
		return Quote{}, fmt.Errorf("quote: details.currencyOut is not Solana USDC")
	}
	if d.CurrencyIn.Currency.ChainID != net.ChainID || !strings.EqualFold(d.CurrencyIn.Currency.Address, tok.Address) {
		return Quote{}, fmt.Errorf("quote: details.currencyIn is not %s on %s", req.Asset, req.Chain)
	}
	in, ok := parseAmount(d.CurrencyIn.Amount)
	if !ok || in.Cmp(req.AmountRaw) != 0 {
		return Quote{}, fmt.Errorf("quote: details.currencyIn.amount %q is not the requested amount", d.CurrencyIn.Amount)
	}
	out, ok := parseAmount(d.CurrencyOut.Amount)
	if !ok || out.Sign() <= 0 {
		return Quote{}, fmt.Errorf("quote: details.currencyOut.amount %q is not positive", d.CurrencyOut.Amount)
	}
	outMin, ok := parseAmount(d.CurrencyOut.MinimumAmount)
	if !ok || outMin.Sign() <= 0 || outMin.Cmp(out) > 0 {
		return Quote{}, fmt.Errorf("quote: details.currencyOut.minimumAmount %q is invalid", d.CurrencyOut.MinimumAmount)
	}
	if rq.RequestID == "" {
		return Quote{}, fmt.Errorf("quote: empty requestId")
	}

	native := tok.Address == NativeToken
	type step struct {
		id string
		tx struct{ from, to, data, value string }
	}
	var steps []step
	for i, s := range rq.Steps {
		if s.Kind != "transaction" {
			return Quote{}, fmt.Errorf("quote: steps[%d].kind %q is not transaction", i, s.Kind)
		}
		if s.RequestID != rq.RequestID {
			return Quote{}, fmt.Errorf("quote: steps[%d].requestId differs from requestId", i)
		}
		if len(s.Items) != 1 {
			return Quote{}, fmt.Errorf("quote: steps[%d] has %d items, want 1", i, len(s.Items))
		}
		t := s.Items[0].Data
		if t.ChainID != net.ChainID {
			return Quote{}, fmt.Errorf("quote: steps[%d].chainId %d is not %d", i, t.ChainID, net.ChainID)
		}
		if !strings.EqualFold(t.From, req.From) {
			return Quote{}, fmt.Errorf("quote: steps[%d].from is not the user's wallet", i)
		}
		if !isAddress(t.To) {
			return Quote{}, fmt.Errorf("quote: steps[%d].to is not an address", i)
		}
		if _, err := decodeHex(t.Data); err != nil {
			return Quote{}, fmt.Errorf("quote: steps[%d].data: %w", i, err)
		}
		if _, ok := parseAmount(t.Value); !ok {
			return Quote{}, fmt.Errorf("quote: steps[%d].value %q is not a decimal amount", i, t.Value)
		}
		st := step{id: s.ID}
		st.tx.from, st.tx.to, st.tx.data, st.tx.value = t.From, t.To, t.Data, t.Value
		steps = append(steps, st)
	}

	var approve *step
	var deposit step
	switch {
	case len(steps) == 1 && steps[0].id == "deposit":
		deposit = steps[0]
	case len(steps) == 2 && steps[0].id == "approve" && steps[1].id == "deposit":
		approve, deposit = &steps[0], steps[1]
	default:
		ids := make([]string, len(steps))
		for i, s := range steps {
			ids[i] = s.id
		}
		return Quote{}, fmt.Errorf("quote: steps %v are not [deposit] or [approve deposit]", ids)
	}

	if !strings.EqualFold(deposit.tx.to, relayDepository) {
		return Quote{}, fmt.Errorf("quote: deposit target %s is not Relay's deposit contract", deposit.tx.to)
	}
	if err := checkDeposit(deposit.tx.data, req.From, tok, req.AmountRaw); err != nil {
		return Quote{}, fmt.Errorf("quote: deposit data: %w", err)
	}
	value, _ := parseAmount(deposit.tx.value)
	if native {
		if approve != nil {
			return Quote{}, fmt.Errorf("quote: steps has an approve for the native coin")
		}
		if value.Cmp(req.AmountRaw) != 0 {
			return Quote{}, fmt.Errorf("quote: deposit value %s is not the requested amount", deposit.tx.value)
		}
	} else {
		if value.Sign() != 0 {
			return Quote{}, fmt.Errorf("quote: deposit value %s must be 0 for a token", deposit.tx.value)
		}
		if approve == nil {
			return Quote{}, fmt.Errorf("quote: a %s deposit needs an approve step", req.Asset)
		}
		if v, _ := parseAmount(approve.tx.value); v.Sign() != 0 {
			return Quote{}, fmt.Errorf("quote: approve value %s must be 0", approve.tx.value)
		}
		if !strings.EqualFold(approve.tx.to, tok.Address) {
			return Quote{}, fmt.Errorf("quote: approve target is not the %s contract", req.Asset)
		}
		if err := checkApprove(approve.tx.data, deposit.tx.to, req.AmountRaw); err != nil {
			return Quote{}, fmt.Errorf("quote: approve data: %w", err)
		}
	}

	q := Quote{
		RequestID:       rq.RequestID,
		AmountInRaw:     in,
		AmountOutRaw:    out,
		AmountOutMinRaw: outMin,
		AmountInUSD:     d.CurrencyIn.AmountUSD,
		GasUSD:          rq.Fees.Gas.AmountUSD,
		RelayerUSD:      rq.Fees.Relayer.AmountUSD,
		ImpactUSD:       d.TotalImpact.USD,
		ImpactPercent:   d.TotalImpact.Percent,
	}
	if d.TimeEstimate != nil && *d.TimeEstimate >= 0 {
		eta := int64(math.Ceil(*d.TimeEstimate))
		q.ETASeconds = &eta
	}
	if approve != nil {
		q.Calls = append(q.Calls, Call{To: approve.tx.to, Data: approve.tx.data, Value: approve.tx.value})
	}
	q.Calls = append(q.Calls, Call{To: deposit.tx.to, Data: deposit.tx.data, Value: deposit.tx.value})
	return q, nil
}

// checkApprove accepts only approve(spender, amount) with the spender and
// amount we expect: selector, then two 32-byte words, nothing else.
func checkApprove(data, spender string, amount *big.Int) error {
	b, _ := decodeHex(data)
	if len(b) != 4+64 || hex.EncodeToString(b[:4]) != approveSelector {
		return fmt.Errorf("not approve(spender, amount)")
	}
	if err := checkAddressWord(b[4:36], spender); err != nil {
		return fmt.Errorf("spender %w", err)
	}
	if new(big.Int).SetBytes(b[36:]).Cmp(amount) != 0 {
		return fmt.Errorf("approved amount is not the requested amount")
	}
	return nil
}

// checkDeposit accepts only depositErc20(depositor, token, amount, id) or
// depositNative(depositor, id) with the depositor, token and amount we expect.
// The trailing id word is Relay's own and is not checked.
func checkDeposit(data, depositor string, tok Token, amount *big.Int) error {
	b, _ := decodeHex(data)
	if tok.Address == NativeToken {
		if len(b) != 4+2*32 || hex.EncodeToString(b[:4]) != depositNativeSelector {
			return fmt.Errorf("not depositNative(depositor, id)")
		}
	} else {
		if len(b) != 4+4*32 || hex.EncodeToString(b[:4]) != depositErc20Selector {
			return fmt.Errorf("not depositErc20(depositor, token, amount, id)")
		}
		if err := checkAddressWord(b[36:68], tok.Address); err != nil {
			return fmt.Errorf("token %w", err)
		}
		if new(big.Int).SetBytes(b[68:100]).Cmp(amount) != 0 {
			return fmt.Errorf("amount is not the requested amount")
		}
	}
	if err := checkAddressWord(b[4:36], depositor); err != nil {
		return fmt.Errorf("depositor %w", err)
	}
	return nil
}

func checkAddressWord(word []byte, want string) error {
	for _, z := range word[:12] {
		if z != 0 {
			return fmt.Errorf("word is not an address")
		}
	}
	if !strings.EqualFold("0x"+hex.EncodeToString(word[12:]), want) {
		return fmt.Errorf("is not the expected address")
	}
	return nil
}

func parseAmount(s string) (*big.Int, bool) {
	if s == "" {
		return nil, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok
}

func isAddress(s string) bool {
	if len(s) != 42 || s[:2] != "0x" {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

func decodeHex(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "0x") {
		return nil, fmt.Errorf("missing 0x prefix")
	}
	return hex.DecodeString(s[2:])
}
