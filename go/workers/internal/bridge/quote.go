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
// them. minCredit is set only for a swap through Relay's approval proxy. It is
// the swap floor in the origin chain's USDC units, the least the simulation
// must see the depository credit the user.
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
	minCredit       *big.Int
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
	approveSelector              = "095ea7b3"
	depositErc20Selector         = "e8017952"
	transferAndMulticallSelector = "f9e4bab4"
	cleanupErc20sViaCallSelector = "73b7bb2f"
	depositAllowanceSelector     = "5a1ee3ac"
)

// relayDepository is the contract every live Relay quote deposits into. It is
// one address on every chain; Sourcify verifies it as RelayDepository on
// ethereum, bsc, base, arbitrum, optimism and polygon (checked 2026-11-02).
const relayDepository = "0x4cd00e387622c35bddb9b4c962c136462338bc31"

// relayApprovalProxy and relayRouter are Relay's v3.2 approvalProxy and
// erc20Router on ethereum, bsc, base, arbitrum, optimism and polygon, the
// chains with swap routes. The proxy hands the user's tokens only to that
// router, an immutable of its verified source (checked 2026-11-02).
const (
	relayApprovalProxy = "0x5567954cd026cebfe486062cb75848e0f1c12e58"
	relayRouter        = "0x0729e2a4716d9f7dd17858d5d504426d228697f7"
)

const solanaUSDCDecimals = 6

const maxSwapShortfallBps = 400

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

// CheckQuote parses Relay's response, which is untrusted, and checks its
// structure only. The transactions must be the user's own, on the right chain,
// and spend exactly the quoted amount of the requested token, and any approve
// must be for exactly that amount; Relay omits it when the user's allowance
// already covers the amount. The quote's recipient and output must be the ones
// requested. That fully
// binds a direct deposit, whose depositErc20 names the user, the token and the
// amount. A swap through Relay's approval proxy is bound only around the swap.
// The proxy hands the amount to Relay's router, the router approves and calls
// a swapper whose calldata is opaque, and then deposits whatever USDC it holds
// for the user. Relay's minimum output must reach the swap floor, set from the
// requested amount, and Relay.Quote simulates the calls to check the deposit
// credits the user at least that floor. The simulation skips validation and
// overrides no block fields, so a swapper able to tell it from a real block
// could behave differently on chain.
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
	var minCredit *big.Int
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

	switch {
	case strings.EqualFold(deposit.tx.to, relayDepository):
		err = checkDeposit(deposit.tx.data, req.From, tok, req.AmountRaw)
	case strings.EqualFold(deposit.tx.to, relayApprovalProxy):
		usdc, ok := net.Token(USDC)
		if !ok || usdc.Decimals < solanaUSDCDecimals {
			return Quote{}, fmt.Errorf("quote: %s has no USDC for Relay's router to deposit", req.Chain)
		}
		if tok.Decimals < solanaUSDCDecimals {
			return Quote{}, fmt.Errorf("quote: %s has fewer decimals than USDC", req.Asset)
		}
		err = checkProxyDeposit(deposit.tx.data, req.From, tok, usdc, req.AmountRaw)
		floor := swapFloor(req.AmountRaw, tok.Decimals)
		if outMin.Cmp(floor) < 0 {
			return Quote{}, fmt.Errorf("quote: details.currencyOut.minimumAmount %s is below the %s USDC floor", outMin, floor)
		}
		minCredit = new(big.Int).Mul(floor, pow10(usdc.Decimals-solanaUSDCDecimals))
	default:
		return Quote{}, fmt.Errorf("quote: deposit target %s is not Relay's deposit contract or approval proxy", deposit.tx.to)
	}
	if err != nil {
		return Quote{}, fmt.Errorf("quote: deposit data: %w", err)
	}
	value, _ := parseAmount(deposit.tx.value)
	if value.Sign() != 0 {
		return Quote{}, fmt.Errorf("quote: deposit value %s must be 0 for a token", deposit.tx.value)
	}
	if approve != nil {
		if v, _ := parseAmount(approve.tx.value); v.Sign() != 0 {
			return Quote{}, fmt.Errorf("quote: approve value %s must be 0", approve.tx.value)
		}
		if !strings.EqualFold(approve.tx.to, tok.Address) {
			return Quote{}, fmt.Errorf("quote: approve target is not the %s contract", req.Asset)
		}
		approveData, _ := decodeHex(approve.tx.data)
		if err := checkApprove(approveData, deposit.tx.to, req.AmountRaw); err != nil {
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
		minCredit:       minCredit,
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

// swapFloor is the least Solana USDC a swap of amount may deliver. Every swap
// asset is a USD stablecoin, so the amount is taken 1:1 in USD, less
// maxSwapShortfallBps.
func swapFloor(amount *big.Int, decimals uint8) *big.Int {
	expected := new(big.Int).Quo(amount, pow10(decimals-solanaUSDCDecimals))
	expected.Mul(expected, big.NewInt(10000-maxSwapShortfallBps))
	return expected.Quo(expected, big.NewInt(10000))
}

func pow10(n uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// checkApprove accepts only approve(spender, amount) with the spender and
// amount we expect: selector, then two 32-byte words, nothing else.
func checkApprove(b []byte, spender string, amount *big.Int) error {
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

// checkDeposit accepts only depositErc20(depositor, token, amount, id) with
// the depositor, token and amount we expect. The trailing id word is Relay's
// own and is not checked.
func checkDeposit(data, depositor string, tok Token, amount *big.Int) error {
	b, _ := decodeHex(data)
	if len(b) != 4+4*32 || hex.EncodeToString(b[:4]) != depositErc20Selector {
		return fmt.Errorf("not depositErc20(depositor, token, amount, id)")
	}
	if err := checkAddressWord(b[36:68], tok.Address); err != nil {
		return fmt.Errorf("token %w", err)
	}
	if new(big.Int).SetBytes(b[68:100]).Cmp(amount) != 0 {
		return fmt.Errorf("amount is not the requested amount")
	}
	if err := checkAddressWord(b[4:36], depositor); err != nil {
		return fmt.Errorf("depositor %w", err)
	}
	return nil
}

// checkProxyDeposit accepts only transferAndMulticall(tokens, amounts, calls,
// refundTo, nftRecipient, metadata) that hands exactly the amount of the token
// to Relay's router, and has the router approve a swapper for that amount, run
// the swap, then deposit its whole USDC balance for the depositor. The swap's
// calldata is Relay's route and is not checked; refundTo, nftRecipient and
// metadata are Relay's own.
func checkProxyDeposit(data, depositor string, tok, usdc Token, amount *big.Int) error {
	b, _ := decodeHex(data)
	if len(b) < 4 || hex.EncodeToString(b[:4]) != transferAndMulticallSelector {
		return fmt.Errorf("not transferAndMulticall(tokens, amounts, calls, refundTo, nftRecipient, metadata)")
	}
	args := abiData(b[4:])
	tokens, err := args.list(0, 1)
	if err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	if got, err := tokens.address(0); err != nil || !strings.EqualFold(got, tok.Address) {
		return fmt.Errorf("token is not the requested token")
	}
	amounts, err := args.list(1, 1)
	if err != nil {
		return fmt.Errorf("amounts: %w", err)
	}
	if got, err := amounts.uint(0); err != nil || got.Cmp(amount) != 0 {
		return fmt.Errorf("amount is not the requested amount")
	}
	list, err := args.list(2, 3)
	if err != nil {
		return fmt.Errorf("router calls: %w", err)
	}
	var calls [3]routerCall
	for i := range calls {
		if calls[i], err = readRouterCall(list, i); err != nil {
			return fmt.Errorf("router call %d: %w", i, err)
		}
	}
	approve, swap, settle := calls[0], calls[1], calls[2]
	if !strings.EqualFold(approve.target, tok.Address) {
		return fmt.Errorf("router call 0 is not to the token")
	}
	if err := checkApprove(approve.data, swap.target, amount); err != nil {
		return fmt.Errorf("router approve: %w", err)
	}
	if strings.EqualFold(swap.target, tok.Address) || strings.EqualFold(swap.target, relayRouter) {
		return fmt.Errorf("swap target %s is the token or the router", swap.target)
	}
	if !strings.EqualFold(settle.target, relayRouter) {
		return fmt.Errorf("router call 2 is not to the router")
	}
	return checkSettle(settle.data, depositor, usdc)
}

type routerCall struct {
	target string
	data   []byte
}

// readRouterCall reads element i of a Call3Value(target, allowFailure,
// value, callData) array.
func readRouterCall(list abiData, i int) (routerCall, error) {
	c, err := list.at(i)
	if err != nil {
		return routerCall{}, err
	}
	target, err := c.address(0)
	if err != nil {
		return routerCall{}, err
	}
	if v, err := c.uint(1); err != nil || v.Sign() != 0 {
		return routerCall{}, fmt.Errorf("allowFailure is set")
	}
	if v, err := c.uint(2); err != nil || v.Sign() != 0 {
		return routerCall{}, fmt.Errorf("sends native value")
	}
	data, err := c.bytes(3)
	if err != nil {
		return routerCall{}, err
	}
	return routerCall{target: target, data: data}, nil
}

// checkSettle accepts only cleanupErc20sViaCall([usdc], [depository],
// [depositErc20(depositor, usdc, id)], [0]), which deposits the router's whole
// USDC balance into Relay's depository for the depositor.
func checkSettle(b []byte, depositor string, usdc Token) error {
	if len(b) < 4 || hex.EncodeToString(b[:4]) != cleanupErc20sViaCallSelector {
		return fmt.Errorf("router call 2 is not cleanupErc20sViaCall(tokens, tos, datas, amounts)")
	}
	args := abiData(b[4:])
	tokens, err := args.list(0, 1)
	if err != nil {
		return fmt.Errorf("settle tokens: %w", err)
	}
	if got, err := tokens.address(0); err != nil || !strings.EqualFold(got, usdc.Address) {
		return fmt.Errorf("settle token is not USDC")
	}
	tos, err := args.list(1, 1)
	if err != nil {
		return fmt.Errorf("settle targets: %w", err)
	}
	if got, err := tos.address(0); err != nil || !strings.EqualFold(got, relayDepository) {
		return fmt.Errorf("settle target is not Relay's deposit contract")
	}
	amounts, err := args.list(3, 1)
	if err != nil {
		return fmt.Errorf("settle amounts: %w", err)
	}
	if got, err := amounts.uint(0); err != nil || got.Sign() != 0 {
		return fmt.Errorf("settle amount is not the router's whole balance")
	}
	datas, err := args.list(2, 1)
	if err != nil {
		return fmt.Errorf("settle datas: %w", err)
	}
	call, err := datas.bytes(0)
	if err != nil {
		return fmt.Errorf("settle data: %w", err)
	}
	if len(call) != 4+3*32 || hex.EncodeToString(call[:4]) != depositAllowanceSelector {
		return fmt.Errorf("settle data is not depositErc20(depositor, token, id)")
	}
	if err := checkAddressWord(call[36:68], usdc.Address); err != nil {
		return fmt.Errorf("settle deposit token %w", err)
	}
	if err := checkAddressWord(call[4:36], depositor); err != nil {
		return fmt.Errorf("depositor %w", err)
	}
	return nil
}

// abiData is Solidity ABI encoding read the way the EVM decodes calldata: a
// dynamic value's head word holds its offset from the start of the enclosing
// tuple.
type abiData []byte

func (d abiData) word(i int) ([]byte, error) {
	if len(d) < 32*(i+1) {
		return nil, fmt.Errorf("word %d past the end", i)
	}
	return d[32*i : 32*(i+1)], nil
}

func (d abiData) uint(i int) (*big.Int, error) {
	w, err := d.word(i)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(w), nil
}

func (d abiData) address(i int) (string, error) {
	w, err := d.word(i)
	if err != nil {
		return "", err
	}
	for _, z := range w[:12] {
		if z != 0 {
			return "", fmt.Errorf("word %d is not an address", i)
		}
	}
	return "0x" + hex.EncodeToString(w[12:]), nil
}

func (d abiData) at(i int) (abiData, error) {
	off, err := d.uint(i)
	if err != nil {
		return nil, err
	}
	if !off.IsInt64() || off.Int64() > int64(len(d)) {
		return nil, fmt.Errorf("offset in word %d past the end", i)
	}
	return d[off.Int64():], nil
}

func (d abiData) list(i, n int) (abiData, error) {
	a, err := d.at(i)
	if err != nil {
		return nil, err
	}
	length, err := a.uint(0)
	if err != nil {
		return nil, err
	}
	if !length.IsInt64() || length.Int64() != int64(n) {
		return nil, fmt.Errorf("%s elements, want %d", length, n)
	}
	return a[32:], nil
}

func (d abiData) bytes(i int) ([]byte, error) {
	a, err := d.at(i)
	if err != nil {
		return nil, err
	}
	length, err := a.uint(0)
	if err != nil {
		return nil, err
	}
	if !length.IsInt64() || length.Int64() > int64(len(a)-32) {
		return nil, fmt.Errorf("bytes in word %d past the end", i)
	}
	return a[32 : 32+length.Int64()], nil
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
