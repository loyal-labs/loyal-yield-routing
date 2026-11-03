package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/mr-tron/base58"
)

// DepositRoute is a chain served by a Relay open deposit address instead of a
// transaction the user signs. It stays out of Networks, because every entry
// there becomes a Privy deposit source and Privy refuses these chains.
type DepositRoute struct {
	Chain        Chain
	Name         string
	Icon         string
	RelayChainID int64
	Asset        Asset
	Token        string
	Decimals     uint8
	MinUSD       int
	// Recovery is Loyal's ops address on the origin chain, which Relay pays
	// only when it can neither fill nor refund.
	Recovery string
}

var Tron = DepositRoute{
	Chain:        "tron",
	Name:         "Tron (TRC20)",
	Icon:         "tron.png",
	RelayChainID: 728126428,
	Asset:        USDT,
	Token:        "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
	Decimals:     6,
	MinUSD:       100,
	Recovery:     "THaGhMBWGmnSfDYRkWdqNZmxACYVVV9ytv",
}

// DepositAddressLead is how long before its order deadline an address stops
// being handed out, so nobody is shown an address about to expire.
const DepositAddressLead = 30 * 24 * time.Hour

// Relay sets the order deadline 180 days out; the slack absorbs clock skew.
const depositOrderMaxLife = 200 * 24 * time.Hour

const tronZeroAddress = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"

const (
	depositSlippageBps  = 50
	depositShortfallBps = 100
	depositFeeAllowance = 4_000_000
	solanaUSDCDigits    = 6
)

var plainDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type DepositAddress struct {
	Address     string
	RequestID   string
	ExpiresAt   time.Time
	FeeUSD      string
	ReceiveUSDC string
}

type RefusedOrder struct{ reason string }

func (e *RefusedOrder) Error() string { return "relay deposit order: " + e.reason }

func refuseOrder(format string, args ...any) error {
	return &RefusedOrder{fmt.Sprintf(format, args...)}
}

func (r DepositRoute) minimumRaw() *big.Int {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.Decimals)), nil)
	return scale.Mul(scale, big.NewInt(int64(r.MinUSD)))
}

// DepositAddress mints an open deposit address on the route that pays USDC on
// Solana to recipient. The quote is for the route's minimum; the address takes
// any amount, and Relay requotes each deposit.
func (r *Relay) DepositAddress(ctx context.Context, route DepositRoute, recipient string) (DepositAddress, error) {
	body, err := json.Marshal(map[string]any{
		"user":                tronZeroAddress,
		"recipient":           recipient,
		"recoveryAddress":     route.Recovery,
		"originChainId":       route.RelayChainID,
		"destinationChainId":  SolanaRelayChainID,
		"originCurrency":      route.Token,
		"destinationCurrency": SolanaUSDCMint,
		"amount":              route.minimumRaw().String(),
		"tradeType":           "EXACT_INPUT",
		"useDepositAddress":   true,
		"slippageTolerance":   strconv.Itoa(depositSlippageBps),
	})
	if err != nil {
		return DepositAddress{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/quote/v2", bytes.NewReader(body))
	if err != nil {
		return DepositAddress{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	raw, err := r.do(hreq)
	if err != nil {
		return DepositAddress{}, fmt.Errorf("relay deposit address: %w", err)
	}
	return CheckDepositOrder(route, recipient, raw, time.Now())
}

type relayDepositQuote struct {
	Steps []struct {
		ID             string `json:"id"`
		Kind           string `json:"kind"`
		RequestID      string `json:"requestId"`
		DepositAddress string `json:"depositAddress"`
	} `json:"steps"`
	Fees struct {
		Relayer relayCurrencyAmount `json:"relayer"`
	} `json:"fees"`
	Details struct {
		Recipient   string              `json:"recipient"`
		CurrencyOut relayCurrencyAmount `json:"currencyOut"`
	} `json:"details"`
	Protocol struct {
		V2 struct {
			OrderData struct {
				Inputs []struct {
					Payment struct {
						ChainID  string `json:"chainId"`
						Currency string `json:"currency"`
						Amount   string `json:"amount"`
					} `json:"payment"`
					Refunds []struct {
						ChainID   string `json:"chainId"`
						Recipient string `json:"recipient"`
						Currency  string `json:"currency"`
						Deadline  int64  `json:"deadline"`
						ExtraData string `json:"extraData"`
					} `json:"refunds"`
				} `json:"inputs"`
				Output struct {
					ChainID  string `json:"chainId"`
					Payments []struct {
						Recipient     string `json:"recipient"`
						Currency      string `json:"currency"`
						MinimumAmount string `json:"minimumAmount"`
					} `json:"payments"`
					Calls     []json.RawMessage `json:"calls"`
					Deadline  int64             `json:"deadline"`
					ExtraData string            `json:"extraData"`
				} `json:"output"`
				Fees []json.RawMessage `json:"fees"`
			} `json:"orderData"`
		} `json:"v2"`
	} `json:"protocol"`
}

// CheckDepositOrder parses Relay's deposit-address quote, which is untrusted,
// and refuses any order that could pay or refund a deposit anywhere but
// recipient's USDC on Solana.
func CheckDepositOrder(route DepositRoute, recipient string, raw []byte, now time.Time) (DepositAddress, error) {
	var q relayDepositQuote
	if err := json.Unmarshal(raw, &q); err != nil {
		return DepositAddress{}, fmt.Errorf("relay deposit order: %w", err)
	}
	if len(q.Steps) != 1 || q.Steps[0].ID != "deposit" || q.Steps[0].Kind != "transaction" {
		return DepositAddress{}, refuseOrder("steps are not one deposit transaction")
	}
	step := q.Steps[0]
	if step.RequestID == "" {
		return DepositAddress{}, refuseOrder("empty requestId")
	}
	if !isTronAddress(step.DepositAddress) || step.DepositAddress == tronZeroAddress {
		return DepositAddress{}, refuseOrder("depositAddress %q is not a Tron address", step.DepositAddress)
	}
	if q.Details.Recipient != recipient {
		return DepositAddress{}, refuseOrder("details.recipient %q is not the requested recipient", q.Details.Recipient)
	}
	order := q.Protocol.V2.OrderData
	if len(order.Inputs) != 1 {
		return DepositAddress{}, refuseOrder("%d inputs, want 1", len(order.Inputs))
	}
	in := order.Inputs[0]
	if in.Payment.ChainID != string(route.Chain) || in.Payment.Currency != route.Token {
		return DepositAddress{}, refuseOrder("input is not %s on %s", route.Asset, route.Chain)
	}
	amount, ok := parseAmount(in.Payment.Amount)
	if !ok || amount.Cmp(route.minimumRaw()) != 0 {
		return DepositAddress{}, refuseOrder("input amount %q is not the requested amount", in.Payment.Amount)
	}
	if len(in.Refunds) != 1 {
		return DepositAddress{}, refuseOrder("%d refund legs, want 1", len(in.Refunds))
	}
	if r := in.Refunds[0]; r.ChainID != "solana" || r.Recipient != recipient || r.Currency != SolanaUSDCMint || r.ExtraData != "0x" {
		return DepositAddress{}, refuseOrder("refund pays %s %s on %s with extraData %q, not USDC to the requested recipient on solana", r.Currency, r.Recipient, r.ChainID, r.ExtraData)
	}
	out := order.Output
	if out.ChainID != "solana" || len(out.Payments) != 1 {
		return DepositAddress{}, refuseOrder("output is not one payment on solana")
	}
	p := out.Payments[0]
	if p.Recipient != recipient || p.Currency != SolanaUSDCMint {
		return DepositAddress{}, refuseOrder("output pays %s %s, not USDC to the requested recipient", p.Currency, p.Recipient)
	}
	if least, ok := parseAmount(p.MinimumAmount); !ok || least.Sign() <= 0 || least.Cmp(route.outputFloor(amount)) < 0 {
		return DepositAddress{}, refuseOrder("output minimumAmount %q is below the %s USDC floor", p.MinimumAmount, route.outputFloor(amount))
	}
	if len(out.Calls) != 0 || len(order.Fees) != 0 || out.ExtraData != "0x" {
		return DepositAddress{}, refuseOrder("order carries %d output calls, %d fees and extraData %q", len(out.Calls), len(order.Fees), out.ExtraData)
	}
	deadline := time.Unix(out.Deadline, 0).UTC()
	if !deadline.After(now.Add(DepositAddressLead)) || deadline.After(now.Add(depositOrderMaxLife)) {
		return DepositAddress{}, refuseOrder("deadline %s is not between %s and %s from now", deadline.Format(time.RFC3339), DepositAddressLead, depositOrderMaxLife)
	}
	if refund := in.Refunds[0].Deadline; refund < out.Deadline {
		return DepositAddress{}, refuseOrder("refund deadline %d is before the order deadline %d", refund, out.Deadline)
	}
	return DepositAddress{
		Address:     step.DepositAddress,
		RequestID:   step.RequestID,
		ExpiresAt:   deadline,
		FeeUSD:      route.shownFee(q.Fees.Relayer.AmountUSD),
		ReceiveUSDC: route.shownReceive(q.Details.CurrencyOut, amount),
	}, nil
}

// outputFloor is the least Solana USDC an order may pay for inputRaw: the
// input taken 1:1 in USD, less depositShortfallBps and Relay's fixed fee,
// allowed up to depositFeeAllowance.
func (r DepositRoute) outputFloor(inputRaw *big.Int) *big.Int {
	floor := new(big.Int).Mul(inputRaw, big.NewInt(10000-depositShortfallBps))
	floor.Quo(floor, big.NewInt(10000))
	floor.Quo(floor, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.Decimals)-solanaUSDCDigits), nil))
	return floor.Sub(floor, big.NewInt(depositFeeAllowance))
}

// shownFee is Relay's fee in USD when it is a plain decimal no larger than
// the route minimum; anything else is not shown.
func (r DepositRoute) shownFee(usd string) string {
	if !plainDecimal.MatchString(usd) {
		return ""
	}
	fee, ok := new(big.Rat).SetString(usd)
	if !ok || fee.Cmp(new(big.Rat).SetInt64(int64(r.MinUSD))) > 0 {
		return ""
	}
	return usd
}

func (r DepositRoute) shownReceive(out relayCurrencyAmount, inputRaw *big.Int) string {
	if out.Currency.ChainID != SolanaRelayChainID || out.Currency.Address != SolanaUSDCMint {
		return ""
	}
	amount, ok := parseAmount(out.Amount)
	ceiling := new(big.Int).Quo(inputRaw, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.Decimals)-solanaUSDCDigits), nil))
	if !ok || amount.Sign() <= 0 || amount.Cmp(ceiling) > 0 {
		return ""
	}
	return new(big.Rat).SetFrac(amount, new(big.Int).Exp(big.NewInt(10), big.NewInt(solanaUSDCDigits), nil)).FloatString(solanaUSDCDigits)
}

func isTronAddress(s string) bool {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 25 || b[0] != 0x41 {
		return false
	}
	first := sha256.Sum256(b[:21])
	second := sha256.Sum256(first[:])
	return bytes.Equal(second[:4], b[21:])
}
