package bridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func solanaOutQuote(t *testing.T) (OutRequest, map[string]any) {
	return OutRequest{
		Chain:     Base,
		Asset:     USDC,
		AmountRaw: big.NewInt(25_000_000),
		Sender:    solana.MustPublicKeyFromBase58("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"),
		Recipient: "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7",
	}, recordedQuote(t, "relay-quote-solana-base-usdc.json")
}

func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

func setAt(root map[string]any, value any, path ...any) {
	parent := at(root, path[:len(path)-1]...)
	switch k := path[len(path)-1].(type) {
	case string:
		parent.(map[string]any)[k] = value
	case int:
		parent.([]any)[k] = value
	}
}

func TestCheckOutQuoteRefusesWhatRelayMustNotChange(t *testing.T) {
	ixPath := []any{"steps", 0, "items", 0, "data", "instructions", 0}
	with := func(tail ...any) []any { return append(append([]any{}, ixPath...), tail...) }
	setData := func(q map[string]any, edit func(b []byte)) {
		b, _ := hex.DecodeString(at(q, with("data")...).(string))
		edit(b)
		setAt(q, hex.EncodeToString(b), with("data")...)
	}
	const usdtMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	const elsewhere = "11111111111111111111111111111112"

	req, base := solanaOutQuote(t)
	raw, _ := json.Marshal(base)
	if _, err := CheckOutQuote(req, raw); err != nil {
		t.Fatalf("recorded quote refused: %v", err)
	}

	cases := map[string]func(q map[string]any){
		"amount bytes plus one": func(q map[string]any) {
			setData(q, func(b []byte) { binary.LittleEndian.PutUint64(b[8:16], binary.LittleEndian.Uint64(b[8:16])+1) })
		},
		"details.currencyIn.amount plus one": func(q map[string]any) { setAt(q, "25000001", "details", "currencyIn", "amount") },
		"key 0 relay depository changed":     func(q map[string]any) { setAt(q, elsewhere, with("keys", 0, "pubkey")...) },
		"key 1 changed":                      func(q map[string]any) { setAt(q, elsewhere, with("keys", 1, "pubkey")...) },
		"key 2 changed":                      func(q map[string]any) { setAt(q, elsewhere, with("keys", 2, "pubkey")...) },
		"key 3 vault changed":                func(q map[string]any) { setAt(q, elsewhere, with("keys", 3, "pubkey")...) },
		"mint key is the USDT mint":          func(q map[string]any) { setAt(q, usdtMint, with("keys", 4, "pubkey")...) },
		"ata key changed":                    func(q map[string]any) { setAt(q, elsewhere, with("keys", 5, "pubkey")...) },
		"vault token account changed":        func(q map[string]any) { setAt(q, elsewhere, with("keys", 6, "pubkey")...) },
		"second key signs":                   func(q map[string]any) { setAt(q, true, with("keys", 3, "isSigner")...) },
		"program id changed":                 func(q map[string]any) { setAt(q, elsewhere, with("programId")...) },
		"discriminator changed":              func(q map[string]any) { setData(q, func(b []byte) { b[0] ^= 1 }) },
		"extra SPL transfer instruction": func(q map[string]any) {
			ixs := at(q, "steps", 0, "items", 0, "data", "instructions").([]any)
			transfer := map[string]any{
				"programId": solana.TokenProgramID.String(),
				"keys": []any{
					map[string]any{"pubkey": at(q, with("keys", 5, "pubkey")...), "isSigner": false, "isWritable": true},
					map[string]any{"pubkey": elsewhere, "isSigner": false, "isWritable": true},
					map[string]any{"pubkey": req.Sender.String(), "isSigner": true, "isWritable": false},
				},
				"data": "0340787d0100000000",
			}
			setAt(q, append(ixs, transfer), "steps", 0, "items", 0, "data", "instructions")
		},
		"details.recipient changed": func(q map[string]any) {
			setAt(q, "0x1111111111111111111111111111111111111111", "details", "recipient")
		},
		"details.sender changed": func(q map[string]any) { setAt(q, elsewhere, "details", "sender") },
		"currencyOut address changed": func(q map[string]any) {
			setAt(q, "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2", "details", "currencyOut", "currency", "address")
		},
		"slippage changed": func(q map[string]any) { setAt(q, "200", "details", "slippageTolerance", "total") },
		"second step": func(q map[string]any) {
			steps := q["steps"].([]any)
			q["steps"] = append(steps, steps[0])
		},
		"step requestId mismatch": func(q map[string]any) { setAt(q, "0x01", "steps", 0, "requestId") },
		"empty requestId": func(q map[string]any) {
			q["requestId"] = ""
			setAt(q, "", "steps", 0, "requestId")
		},
		"step kind is signature": func(q map[string]any) { setAt(q, "signature", "steps", 0, "kind") },
		"minimumAmount above amount": func(q map[string]any) {
			out, _ := new(big.Int).SetString(at(q, "details", "currencyOut", "amount").(string), 10)
			above := out.Add(out, big.NewInt(1)).String()
			setAt(q, above, "details", "currencyOut", "minimumAmount")
			setAt(q, above, "protocol", "v2", "orderData", "output", "payments", 0, "minimumAmount")
		},
		"minimumAmount far below amount": func(q map[string]any) {
			setAt(q, "1", "details", "currencyOut", "minimumAmount")
			setAt(q, "1", "protocol", "v2", "orderData", "output", "payments", 0, "minimumAmount")
		},
		"output pays 1 raw unit for 25 USDC": func(q map[string]any) {
			setAt(q, "1", "details", "currencyOut", "amount")
			setAt(q, "1", "details", "currencyOut", "minimumAmount")
			setAt(q, "1", "protocol", "v2", "orderData", "output", "payments", 0, "minimumAmount")
		},
		"output is half of the input less one": func(q map[string]any) {
			setAt(q, "12499999", "details", "currencyOut", "amount")
			setAt(q, "12499999", "details", "currencyOut", "minimumAmount")
			setAt(q, "12499999", "protocol", "v2", "orderData", "output", "payments", 0, "minimumAmount")
		},
		"order output is on another chain": func(q map[string]any) {
			setAt(q, "ethereum", "protocol", "v2", "orderData", "output", "chainId")
		},
		"order output runs a call": func(q map[string]any) {
			setAt(q, []any{map[string]any{"to": "0x1111111111111111111111111111111111111111", "data": "0x", "value": "0"}}, "protocol", "v2", "orderData", "output", "calls")
		},
		"second refund goes to someone else": func(q map[string]any) {
			setAt(q, "0x1111111111111111111111111111111111111111", "protocol", "v2", "orderData", "inputs", 0, "refunds", 1, "recipient")
		},
		"second refund is on another chain": func(q map[string]any) {
			setAt(q, "ethereum", "protocol", "v2", "orderData", "inputs", 0, "refunds", 1, "chainId")
		},
		"order refunds someone else": func(q map[string]any) {
			setAt(q, elsewhere, "protocol", "v2", "orderData", "inputs", 0, "refunds", 0, "recipient")
		},
		"order pays someone else": func(q map[string]any) {
			setAt(q, "0x1111111111111111111111111111111111111111", "protocol", "v2", "orderData", "output", "payments", 0, "recipient")
		},
		"order id is not the deposit id": func(q map[string]any) { setAt(q, "0x01", "protocol", "v2", "orderId") },
		"unknown lookup table": func(q map[string]any) {
			setAt(q, []any{elsewhere}, "steps", 0, "items", 0, "data", "addressLookupTableAddresses")
		},
		"fees.app removed":             func(q map[string]any) { delete(at(q, "fees").(map[string]any), "app") },
		"fees.app zero":                func(q map[string]any) { setAt(q, "0", "fees", "app", "amount") },
		"fees.app 39 bps":              func(q map[string]any) { setAt(q, "97500", "fees", "app", "amount") },
		"fees.app 41 bps":              func(q map[string]any) { setAt(q, "102500", "fees", "app", "amount") },
		"fees.app two units over":      func(q map[string]any) { setAt(q, "100002", "fees", "app", "amount") },
		"fees.app amount not a number": func(q map[string]any) { setAt(q, "0x186a0", "fees", "app", "amount") },
		"fees.app in USDT": func(q map[string]any) {
			setAt(q, usdtMint, "fees", "app", "currency", "address")
		},
		"fees.app on another chain": func(q map[string]any) {
			setAt(q, float64(8453), "fees", "app", "currency", "chainId")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, q := solanaOutQuote(t)
			mutate(q)
			raw, _ := json.Marshal(q)
			if _, err := CheckOutQuote(req, raw); err == nil {
				t.Fatal("mutated quote accepted")
			}
		})
	}
}

func TestCheckOutQuoteServesTheRateItComputes(t *testing.T) {
	req, q := solanaOutQuote(t)
	setAt(q, "5.0", "details", "rate")
	raw, _ := json.Marshal(q)
	got, err := CheckOutQuote(req, raw)
	if err != nil {
		t.Fatalf("a Relay rate field must not decide the quote: %v", err)
	}
	if got.Rate != "0.994926" {
		t.Fatalf("rate %q is not out over in", got.Rate)
	}
}

func TestCheckOutQuoteUSDFieldsAreDisplayOnly(t *testing.T) {
	req, recorded := solanaOutQuote(t)
	rawRecorded, _ := json.Marshal(recorded)
	want, err := CheckOutQuote(req, rawRecorded)
	if err != nil {
		t.Fatalf("recorded quote refused: %v", err)
	}
	if want.AmountInUSD != "24.995375" || want.AmountOutUSD != "24.868544" || want.NetworkFeeUSD != "0.000921" {
		t.Fatalf("recorded USD fields served as %q %q %q", want.AmountInUSD, want.AmountOutUSD, want.NetworkFeeUSD)
	}
	for _, tampered := range []any{"0", "99999999999999999999.5", "1e999", "NaN", "-5", ""} {
		_, q := solanaOutQuote(t)
		setAt(q, tampered, "details", "currencyIn", "amountUsd")
		setAt(q, tampered, "details", "currencyOut", "amountUsd")
		setAt(q, tampered, "fees", "gas", "amountUsd")
		setAt(q, tampered, "details", "rate")
		raw, _ := json.Marshal(q)
		got, err := CheckOutQuote(req, raw)
		if err != nil {
			t.Fatalf("USD %q decided the quote: %v", tampered, err)
		}
		if got.RequestID != want.RequestID || got.AmountInRaw.Cmp(want.AmountInRaw) != 0 || got.AmountOutRaw.Cmp(want.AmountOutRaw) != 0 || got.AmountOutMinRaw.Cmp(want.AmountOutMinRaw) != 0 || got.Rate != want.Rate {
			t.Fatalf("USD %q changed the decision: %+v vs %+v", tampered, got, want)
		}
		if plainDecimal.MatchString(tampered.(string)) {
			if got.AmountOutUSD != tampered {
				t.Fatalf("decimal %q not passed through, got %q", tampered, got.AmountOutUSD)
			}
		} else if got.AmountInUSD != "" || got.AmountOutUSD != "" || got.NetworkFeeUSD != "" {
			t.Fatalf("non-decimal %q served as %q %q %q, want empty", tampered, got.AmountInUSD, got.AmountOutUSD, got.NetworkFeeUSD)
		}
	}
}

func TestQuoteOutRelayAuthFailureIsNotAUserRefusal(t *testing.T) {
	req, _ := solanaOutQuote(t)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"errorCode":"UNAUTHORIZED"}`))
		}))
		relay := &Relay{BaseURL: srv.URL, HTTP: srv.Client()}
		_, err := relay.QuoteOut(context.Background(), req)
		srv.Close()
		var refused *QuoteRefused
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("relay %d returned %v, want an unavailable error", status, err)
		}
	}
}

func TestQuoteOutAppFee(t *testing.T) {
	req, base := solanaOutQuote(t)
	raw, _ := json.Marshal(base)
	const claim = "0x536a85f63ceca75b12ec588cf37ca371489dd4a3"
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Write(raw)
	}))
	defer srv.Close()
	relay := &Relay{BaseURL: srv.URL, HTTP: srv.Client()}

	if _, err := relay.QuoteOut(context.Background(), req); !errors.Is(err, ErrAppFeeUnset) {
		t.Fatalf("unset recipient: got %v, want ErrAppFeeUnset", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("%d requests reached Relay with no fee recipient", len(bodies))
	}

	relay.AppFeeRecipient = claim
	q, err := relay.QuoteOut(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		AppFees []struct{ Recipient, Fee string } `json:"appFees"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil || len(sent.AppFees) != 1 || sent.AppFees[0].Recipient != claim || sent.AppFees[0].Fee != "40" {
		t.Fatalf("quote body appFees = %s", bodies[0])
	}
	if q.AppFeeRaw.String() != "100000" {
		t.Fatalf("app fee %s, want 100000", q.AppFeeRaw)
	}
}
