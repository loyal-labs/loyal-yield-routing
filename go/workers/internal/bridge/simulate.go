package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

const maxRPCBody = 4 << 20

// relayErc20DepositTopic is keccak256("RelayErc20Deposit(address,address,uint256,bytes32)"),
// the event RelayDepository emits for every ERC-20 deposit, its four fields
// unindexed in the log data.
const relayErc20DepositTopic = "0x49fed1d0b752ce30eee63c7a81133f3363b532fec5d4d7dd1ccfd005de4555e1"

const balanceOfSelector = "70a08231"

// ParseEVMRPCURLs reads chain=url pairs separated by commas or whitespace.
func ParseEVMRPCURLs(s string) (map[Chain]string, error) {
	urls := map[Chain]string{}
	for _, pair := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		name, raw, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("an entry is not chain=url")
		}
		c := Chain(name)
		if _, ok := Lookup(c); !ok {
			return nil, fmt.Errorf("unknown chain %q", name)
		}
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("the %s RPC is not an http(s) URL", name)
		}
		urls[c] = raw
	}
	return urls, nil
}

// proveCredit runs the quote's calls in order in one simulated block on the
// origin chain, from the user's address holding exactly the input amount, and
// refuses the quote unless Relay's depository logs a deposit crediting the user
// with at least q.minCredit of the chain's USDC. The user's balance slot is not
// known per token, so each storage key balanceOf(user) reads is tried in its
// own simulation, and the one whose leading balanceOf call returns the amount
// is the one that counts.
func (r *Relay) proveCredit(ctx context.Context, net Network, req QuoteRequest, tok Token, q Quote) error {
	endpoint := r.EVMRPC[net.Chain]
	if endpoint == "" {
		return fmt.Errorf("quote: no EVM RPC for %s to simulate Relay's swap deposit", net.Chain)
	}
	usdc, _ := net.Token(USDC)
	balanceOf := "0x" + balanceOfSelector + strings.Repeat("0", 24) + strings.ToLower(req.From[2:])

	var lists []struct {
		AccessList []struct {
			Address     string   `json:"address"`
			StorageKeys []string `json:"storageKeys"`
		} `json:"accessList"`
	}
	if err := r.rpc(ctx, endpoint, []rpcCall{{"eth_createAccessList", []any{map[string]string{"to": tok.Address, "data": balanceOf}, "latest"}}}, &lists); err != nil {
		return fmt.Errorf("quote: balance slot: %w", err)
	}
	var keys []string
	for _, e := range lists[0].AccessList {
		if strings.EqualFold(e.Address, tok.Address) {
			keys = append(keys, e.StorageKeys...)
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("quote: balanceOf on %s reads no storage", tok.Address)
	}

	amount := fmt.Sprintf("0x%064x", req.AmountRaw)
	calls := []map[string]string{{"from": req.From, "to": tok.Address, "data": balanceOf}}
	for _, c := range q.Calls {
		v, _ := new(big.Int).SetString(c.Value, 10)
		calls = append(calls, map[string]string{"from": req.From, "to": c.To, "data": c.Data, "value": "0x" + v.Text(16)})
	}
	sims := make([]rpcCall, len(keys))
	for i, k := range keys {
		sims[i] = rpcCall{"eth_simulateV1", []any{map[string]any{
			"blockStateCalls": []any{map[string]any{
				"stateOverrides": map[string]any{tok.Address: map[string]any{"stateDiff": map[string]string{k: amount}}},
				"calls":          calls,
			}},
			"validation": false,
		}, "latest"}}
	}
	var blocks [][]struct {
		Calls []simulatedCall `json:"calls"`
	}
	if err := r.rpc(ctx, endpoint, sims, &blocks); err != nil {
		return fmt.Errorf("quote: simulate: %w", err)
	}
	for _, b := range blocks {
		if len(b) != 1 || len(b[0].Calls) != len(calls) {
			return fmt.Errorf("quote: simulate did not return one block of %d calls", len(calls))
		}
		results := b[0].Calls
		held, err := decodeHex(results[0].ReturnData)
		if err != nil || new(big.Int).SetBytes(held).Cmp(req.AmountRaw) != 0 {
			continue
		}
		for i, c := range results[1:] {
			if c.Status != "0x1" {
				return fmt.Errorf("quote: simulated call %d reverted: %s", i, c.Error.Message)
			}
		}
		credited := new(big.Int)
		for _, c := range results[1:] {
			for _, l := range c.Logs {
				if got, ok := depositCredit(l, req.From, usdc.Address); ok && got.Cmp(credited) > 0 {
					credited = got
				}
			}
		}
		if credited.Cmp(q.minCredit) < 0 {
			return fmt.Errorf("quote: simulated deposit credits %s of %s USDC units owed", credited, q.minCredit)
		}
		return nil
	}
	return fmt.Errorf("quote: no storage key balanceOf reads on %s holds the user's balance", tok.Address)
}

type simulatedCall struct {
	Status     string         `json:"status"`
	ReturnData string         `json:"returnData"`
	Logs       []simulatedLog `json:"logs"`
	Error      struct {
		Message string `json:"message"`
	} `json:"error"`
}

type simulatedLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

func depositCredit(l simulatedLog, depositor, token string) (*big.Int, bool) {
	if !strings.EqualFold(l.Address, relayDepository) || len(l.Topics) != 1 || l.Topics[0] != relayErc20DepositTopic {
		return nil, false
	}
	b, err := decodeHex(l.Data)
	if err != nil || len(b) != 4*32 {
		return nil, false
	}
	if checkAddressWord(b[:32], depositor) != nil || checkAddressWord(b[32:64], token) != nil {
		return nil, false
	}
	return new(big.Int).SetBytes(b[64:96]), true
}

type rpcCall struct {
	method string
	params []any
}

// rpc sends calls as one JSON-RPC batch and decodes the i-th result into
// out's i-th element; out points to a slice.
func (r *Relay) rpc(ctx context.Context, endpoint string, calls []rpcCall, out any) error {
	batch := make([]map[string]any, len(calls))
	for i, c := range calls {
		batch[i] = map[string]any{"jsonrpc": "2.0", "id": i, "method": c.method, "params": c.params}
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	res, err := r.HTTP.Do(hreq)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxRPCBody))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", res.StatusCode)
	}
	var replies []struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &replies); err != nil {
		return fmt.Errorf("not a JSON-RPC batch reply: %w", err)
	}
	if len(replies) != len(calls) {
		return fmt.Errorf("%d replies to %d calls", len(replies), len(calls))
	}
	results := make([]json.RawMessage, len(calls))
	for _, rep := range replies {
		if rep.ID < 0 || rep.ID >= len(calls) || results[rep.ID] != nil {
			return fmt.Errorf("reply id %d", rep.ID)
		}
		if rep.Error != nil {
			return fmt.Errorf("%s: %s", calls[rep.ID].method, rep.Error.Message)
		}
		results[rep.ID] = rep.Result
	}
	joined, _ := json.Marshal(results)
	return json.Unmarshal(joined, out)
}
