package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	loyalCommunityChatID = -1002981429221
	loyalCA              = "LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta"
	jupiterTokenSearch   = "https://lite-api.jup.ag/tokens/v2/search"
)

type optional[T any] struct {
	value T
	ok    bool
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.ok = string(data) != "null" && json.Unmarshal(data, &o.value) == nil
	return nil
}

type jupiterToken struct {
	ID          optional[string]  `json:"id"`
	UsdPrice    optional[float64] `json:"usdPrice"`
	Mcap        optional[float64] `json:"mcap"`
	FDV         optional[float64] `json:"fdv"`
	HolderCount optional[float64] `json:"holderCount"`
	Liquidity   optional[float64] `json:"liquidity"`
	UpdatedAt   optional[string]  `json:"updatedAt"`
}

func (b *Bot) tokenMetrics(ctx context.Context, mint string) (*jupiterToken, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.jupiterSearch+"?"+url.Values{"query": {mint}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := b.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var tokens []jupiterToken
	if err := json.NewDecoder(response.Body).Decode(&tokens); err != nil {
		return nil, err
	}
	for i := range tokens {
		t := &tokens[i]
		if t.ID.ok && t.ID.value == mint {
			if !t.UsdPrice.ok || !t.Mcap.ok {
				return nil, errors.New("Invalid Jupiter token response.")
			}
			return t, nil
		}
	}
	return nil, errors.New("Invalid Jupiter token response.")
}

func groupThousands(digits string) string {
	var out strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func roundedDecimal(v float64, places int) (whole, fraction string) {
	text := strconv.FormatFloat(math.Abs(v), 'f', -1, 64)
	whole, fraction, _ = strings.Cut(text, ".")
	if len(fraction) > places {
		up := fraction[places] >= '5'
		digits := []byte(whole + fraction[:places])
		for i := len(digits) - 1; up && i >= 0; i-- {
			if digits[i] == '9' {
				digits[i] = '0'
			} else {
				digits[i]++
				up = false
			}
		}
		if up {
			digits = append([]byte{'1'}, digits...)
		}
		whole, fraction = string(digits[:len(digits)-places]), string(digits[len(digits)-places:])
	}
	return whole, fraction + strings.Repeat("0", places-len(fraction))
}

func formatNumber(v float64, places int) string {
	whole, fraction := roundedDecimal(v, places)
	out := groupThousands(whole)
	if places > 0 {
		out += "." + fraction
	}
	if math.Signbit(v) {
		out = "-" + out
	}
	return out
}

func finite(o optional[float64]) bool {
	return o.ok && !math.IsInf(o.value, 0) && !math.IsNaN(o.value)
}

func usd(o optional[float64]) string {
	if !finite(o) {
		return "N/A"
	}
	return "$" + formatNumber(o.value, 3)
}

func integer(o optional[float64]) string {
	if !finite(o) {
		return "N/A"
	}
	return formatNumber(o.value, 0)
}

func updatedAt(o optional[string]) string {
	if !o.ok {
		return "N/A"
	}
	units := utf16.Encode([]rune(o.value))
	if len(units) < 19 {
		return "N/A"
	}
	return strings.Replace(string(utf16.Decode(units[:19])), "T", " ", 1)
}

func caMessage(token *jupiterToken) string {
	if token == nil {
		token = &jupiterToken{}
	}
	return "$LOYAL's CA: `" + loyalCA + "`\n\n" + strings.Join([]string{
		"Price: **" + usd(token.UsdPrice) + "**",
		"Market cap: **" + usd(token.Mcap) + "**",
		"FDV: **" + usd(token.FDV) + "**",
		"Holders: **" + integer(token.HolderCount) + "**",
		"Liquidity: **" + usd(token.Liquidity) + "**",
		"Updated: **" + updatedAt(token.UpdatedAt) + "**",
	}, "\n")
}

func (b *Bot) ca(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	if msg.Chat.ID != loyalCommunityChatID {
		return nil
	}
	token, err := b.tokenMetrics(ctx, loyalCA)
	if err != nil {
		slog.Error("Jupiter token fetch for ca command failed", "update_id", update.UpdateID, "error", err)
	}
	keyboard := &Keyboard{Rows: [][]Button{
		{
			{Text: "Jupiter", URL: "https://jup.ag/tokens/" + loyalCA},
			{Text: "Dexscreener", URL: "https://dexscreener.com/solana/" + loyalCA},
		},
		{{Text: "Copy CA to clipboard", CopyText: &CopyText{Text: loyalCA}}},
	}}
	if _, err := b.api.Send(ctx, msg.Chat.ID, caMessage(token), SendOptions{ParseMode: "Markdown", Markup: keyboard}); err != nil {
		return b.sendFailed(update, "/ca", err)
	}
	return nil
}
