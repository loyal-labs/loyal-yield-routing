package bridge

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
)

type Balance struct {
	Chain     Chain
	Asset     Asset
	AmountRaw *big.Int
	Decimals  uint8
}

// PrivyBalanceQueries asks Privy's wallet balance API
// (GET /v1/wallets/{id}/balance) for every token in Networks by contract
// address, at most ten tokens a call. Privy's asset names cover only some of
// the tokens, and it drops a named asset it does not know on a chain.
func PrivyBalanceQueries() []url.Values {
	var out []url.Values
	q := url.Values{}
	for _, n := range Networks {
		for _, t := range n.Tokens {
			if len(q["token"]) == privyBalanceTokensPerCall {
				out = append(out, q)
				q = url.Values{}
			}
			q.Add("token", string(n.Chain)+":"+t.Address)
		}
	}
	return append(out, q)
}

const privyBalanceTokensPerCall = 10

// ParsePrivyBalances returns one Balance per token in Networks, in that order,
// from the answers to PrivyBalanceQueries. A token Privy left out or reported
// with other decimals fails the read, since a missing balance is not a zero
// one.
func ParsePrivyBalances(pages [][]byte) ([]Balance, error) {
	type key struct {
		chain   Chain
		address string
	}
	type value struct {
		raw      string
		decimals int
	}
	got := map[key]value{}
	for _, raw := range pages {
		var res struct {
			Balances []struct {
				Chain    Chain  `json:"chain"`
				Asset    string `json:"asset"`
				RawValue string `json:"raw_value"`
				Decimals int    `json:"raw_value_decimals"`
			} `json:"balances"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("privy balances: %w", err)
		}
		for _, b := range res.Balances {
			got[key{b.Chain, strings.ToLower(b.Asset)}] = value{b.RawValue, b.Decimals}
		}
	}
	var out []Balance
	for _, n := range Networks {
		for _, t := range n.Tokens {
			v, ok := got[key{n.Chain, t.Address}]
			if !ok {
				return nil, fmt.Errorf("privy balances: no %s on %s", t.Asset, n.Chain)
			}
			if v.decimals != int(t.Decimals) {
				return nil, fmt.Errorf("privy balances: %s on %s has %d decimals, want %d", t.Asset, n.Chain, v.decimals, t.Decimals)
			}
			amount, ok := parseAmount(v.raw)
			if !ok {
				return nil, fmt.Errorf("privy balances: %s on %s amount %q is not a decimal integer", t.Asset, n.Chain, v.raw)
			}
			out = append(out, Balance{Chain: n.Chain, Asset: t.Asset, AmountRaw: amount, Decimals: t.Decimals})
		}
	}
	return out, nil
}
