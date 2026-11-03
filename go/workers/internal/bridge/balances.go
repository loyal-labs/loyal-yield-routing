package bridge

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
)

type Balance struct {
	Chain     Chain
	Asset     Asset
	AmountRaw *big.Int
	Decimals  uint8
}

// PrivyBalanceQuery asks Privy's wallet balance API
// (GET /v1/wallets/{id}/balance) for every token in Networks. Privy answers
// each listed asset on each listed chain where it exists.
func PrivyBalanceQuery() url.Values {
	q := url.Values{}
	seen := map[Asset]bool{}
	for _, n := range Networks {
		q.Add("chain", string(n.Chain))
		for _, t := range n.Tokens {
			if !seen[t.Asset] {
				seen[t.Asset] = true
				q.Add("asset", string(t.Asset))
			}
		}
	}
	return q
}

// ParsePrivyBalances returns one Balance per token in Networks, in that order.
// A token Privy left out or reported with other decimals fails the read, since
// a missing balance is not a zero one.
func ParsePrivyBalances(raw []byte) ([]Balance, error) {
	var res struct {
		Balances []struct {
			Chain    Chain  `json:"chain"`
			Asset    Asset  `json:"asset"`
			RawValue string `json:"raw_value"`
			Decimals int    `json:"raw_value_decimals"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("privy balances: %w", err)
	}
	type key struct {
		chain Chain
		asset Asset
	}
	type value struct {
		raw      string
		decimals int
	}
	got := map[key]value{}
	for _, b := range res.Balances {
		got[key{b.Chain, b.Asset}] = value{b.RawValue, b.Decimals}
	}
	var out []Balance
	for _, n := range Networks {
		for _, t := range n.Tokens {
			v, ok := got[key{n.Chain, t.Asset}]
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
